package main

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/jpeg"
	_ "image/png"             // enregistre le décodeur PNG dans le registre image.Decode
	_ "golang.org/x/image/webp" // enregistre le décodeur WebP pour accepter les images WebP en entrée
	"io"
	"net/http"
	"os"
	"runtime"
	"sync"
	"time"

	"github.com/rs/zerolog"
	xdraw "golang.org/x/image/draw"
	"golang.org/x/image/font"
	"golang.org/x/image/font/gofont/goregular"
	"golang.org/x/image/font/opentype"
	"golang.org/x/image/math/fixed"
)

const (
	maxWidth  = 1920 // largeur maximale après resize
	maxHeight = 1080 // hauteur maximale après resize

	maxInputWidth  = 8000 // validation: on refuse les images absurdement grandes
	maxInputHeight = 8000

	wmMargin     = 20 // marge entre le bord de l'image et le texte du watermark (px)
	wmLineHeight = 52 // hauteur de ligne pour la police taille 48 (font size + marge interne)

	// Zone d'échantillonnage pour le calcul de luminosité (pixels autour du watermark).
	// Plus la zone est grande, plus la couleur adaptative est représentative du fond.
	sampleW = 200
	sampleH = 50
)

// sem limite la concurrence à un slot par coeur CPU pour éviter la saturation mémoire
// lors du traitement simultané de plusieurs images volumineuses.
var sem = make(chan struct{}, runtime.NumCPU())

// bufPool réutilise les buffers JPEG/WebP entre les requêtes pour réduire la pression GC.
var bufPool = sync.Pool{
	New: func() any { return new(bytes.Buffer) },
}

// fontFace est la police chargée une seule fois au démarrage et partagée entre toutes les requêtes.
// opentype.Face est thread-safe en lecture.
var fontFace font.Face

// logger est le logger structuré partagé entre toutes les fonctions.
var logger zerolog.Logger

// ── Main ──────────────────────────────────────────────────────────────────────

func main() {
	zerolog.TimeFieldFormat = time.RFC3339 // RFC3339 est plus lisible que l'epoch dans les logs structurés
	// champ "service" identifie ce service dans une stack multi-conteneurs
	logger = zerolog.New(os.Stdout).With().Timestamp().Str("service", "optimizer").Logger()

	numCPU := runtime.NumCPU()                                                                     // loggé au démarrage pour tracer la capacité maximale du worker pool
	logger.Info().Str("addr", ":3001").Int("worker_slots", numCPU).Msg("démarrage")

	if err := loadFont(); err != nil { // la police est critique — impossible de watermarker sans elle
		logger.Fatal().Err(err).Msg("chargement police échoué")
	}

	mux := http.NewServeMux()
	mux.HandleFunc("POST /optimize", handleOptimize) // seule route exposée — le reste est géré par l'API

	http.ListenAndServe(":3001", mux) //nolint:errcheck — une erreur ici est fatale, le conteneur redémarre
}

// ── Handler ───────────────────────────────────────────────────────────────────

// handleOptimize est le handler principal qui orchestre les étapes du pipeline d'optimisation.
func handleOptimize(w http.ResponseWriter, r *http.Request) {
	start := time.Now() // point de référence pour mesurer la durée totale du pipeline

	// ── ① Worker Pool ────────────────────────────────────
	slotsUsed := len(sem) + 1  // +1 car on va acquérir juste après — utile pour détecter la saturation
	totalSlots := cap(sem)     // mis en cache pour le réutiliser dans le defer sans recalcul
	logger.Info().Str("step", "worker_pool").Int("used", slotsUsed).Int("total", totalSlots).Msg("slot acquis")

	sem <- struct{}{} // bloque si tous les slots sont pris — backpressure naturelle sur le client
	defer func() {
		<-sem // libère le slot pour la prochaine requête en attente
		logger.Info().Str("step", "worker_pool").Int("used", len(sem)).Int("total", totalSlots).Msg("slot libéré")
	}()

	// ── ② Décodage (lazy validation + full decode) ────────
	t := time.Now()
	// decodeImage valide d'abord les dimensions via DecodeConfig (sans décoder les pixels),
	// puis effectue le décodage complet. Le ré-encodage ultérieur supprime automatiquement
	// les métadonnées EXIF (GPS, miniature, profil ICC) — gain de 5-15% sur les photos iPhone.
	img, format, err := decodeImage(r)
	if err != nil { // image manquante, format invalide ou dimensions hors limites
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	origW, origH := img.Bounds().Dx(), img.Bounds().Dy() // conservés pour loguer le delta après resize
	logger.Info().Str("step", "decode").Str("format", format).Int("width", origW).Int("height", origH).Dur("duration", time.Since(t)).Msg("décodage + strip EXIF")

	// ── ③ Resize ─────────────────────────────────────────
	t = time.Now()
	resized := resize(img)
	newW, newH := resized.Bounds().Dx(), resized.Bounds().Dy() // nécessaires pour loguer les nouvelles dimensions
	if origW == newW && origH == newH {                         // pas de resize — évite un log trompeur avec durée ~0
		logger.Info().Str("step", "resize").Bool("resized", false).Int("max_w", maxWidth).Int("max_h", maxHeight).Msg("resize ignoré")
	} else {
		logger.Info().Str("step", "resize").Bool("resized", true).Int("from_w", origW).Int("from_h", origH).Int("to_w", newW).Int("to_h", newH).Dur("duration", time.Since(t)).Msg("resize")
	}

	// ── ④ Watermark ──────────────────────────────────────
	t = time.Now()
	wmText, wmPosition := wmParams(r) // extraire les 2 paramètres depuis le formulaire multipart
	watermarked, err := applyWatermark(resized, wmText, wmPosition)
	if err != nil { // échec rare — police corrompue ou canvas non-initialisé
		http.Error(w, "Erreur watermark", http.StatusInternalServerError)
		return
	}
	logger.Info().Str("step", "watermark").Str("text", wmText).Str("position", wmPosition).Dur("duration", time.Since(t)).Msg("watermark appliqué")

	// ── ⑤ Encodage ────────────────────────────────────────
	t = time.Now()
	buf, contentType, q, err := encodeToBuffer(watermarked)
	if err != nil { // échec d'encodage — OOM ou codec indisponible
		http.Error(w, "Erreur encodage", http.StatusInternalServerError)
		return
	}
	defer bufPool.Put(buf) // remettre le buffer dans le pool après que Write() l'ait consommé
	logger.Info().Str("step", "encode").Str("format", "jpeg").Int("quality", q).Str("size", formatBytes(buf.Len())).Dur("duration", time.Since(t)).Msg("encodage")
	logger.Info().Str("step", "total").Dur("duration", time.Since(start)).Msg("image traitée")

	w.Header().Set("Content-Type", contentType) // indique au client comment décoder la réponse (JPEG ou WebP)
	w.Write(buf.Bytes())                         //nolint:errcheck — flush vers le client
}

// ── Pipeline steps ────────────────────────────────────────────────────────────

// decodeImage valide les dimensions via DecodeConfig (sans décoder les pixels),
// puis effectue le décodage complet. Le ré-encodage ultérieur supprime automatiquement
// les métadonnées EXIF (GPS, miniature, profil ICC) — gain de 5-15% sur les photos iPhone.
func decodeImage(r *http.Request) (image.Image, string, error) {
	file, _, err := r.FormFile("image") // on ignore le FileHeader (nom, taille) — on valide via DecodeConfig
	if err != nil {
		return nil, "", fmt.Errorf("image manquante")
	}
	defer file.Close() // libérer la mémoire multipart dès que la fonction retourne

	// ① Lazy decode : lit uniquement le header (quelques Ko) pour valider les dimensions
	// sans décompresser les ~25 millions de pixels d'une image 4K.
	config, format, err := image.DecodeConfig(file)
	if err != nil {
		return nil, "", fmt.Errorf("format invalide")
	}
	if config.Width > maxInputWidth || config.Height > maxInputHeight { // refuse avant décompression pour ne pas saturer la mémoire
		return nil, "", fmt.Errorf("image trop grande (max %dx%d, reçu %dx%d)", maxInputWidth, maxInputHeight, config.Width, config.Height)
	}
	logger.Debug().Str("step", "lazy_decode").Str("format", format).Int("width", config.Width).Int("height", config.Height).Msg("dimensions validées sans décodage pixels")

	// ② Seek back to start before full decode — DecodeConfig a consommé le reader.
	if _, err := file.Seek(0, io.SeekStart); err != nil { // DecodeConfig a avancé le curseur — on revient au début
		return nil, "", fmt.Errorf("seek échoué")
	}

	img, _, err := image.Decode(file) // décodage complet — le second retour (format) est ignoré, déjà lu
	if err != nil {
		return nil, "", fmt.Errorf("décodage échoué")
	}
	return img, format, nil
}

// wmParams lit les paramètres de watermark depuis le formulaire multipart.
// Les valeurs par défaut garantissent un comportement cohérent même si le front
// n'envoie pas ces champs (appels directs à l'API, retry RabbitMQ, etc.).
func wmParams(r *http.Request) (text, position string) {
	text = r.FormValue("wm_text")
	if text == "" {
		text = "NWS © 2026" // fallback si le champ est absent ou vide
	}
	position = r.FormValue("wm_position")
	if position == "" {
		position = "bottom-right" // position la moins intrusive par défaut
	}
	return
}

// encodeToBuffer encode l'image en JPEG dans un buffer recyclé depuis le sync.Pool.
// La qualité est adaptée dynamiquement aux dimensions de l'image de sortie.
// Retourne le buffer, le content-type et la qualité utilisée (pour le log).
// Le caller est responsable de remettre le buffer dans le pool (defer bufPool.Put(buf)).
func encodeToBuffer(img image.Image) (*bytes.Buffer, string, int, error) {
	w, h := img.Bounds().Dx(), img.Bounds().Dy() // dimensions utilisées pour choisir la qualité adaptive
	q := adaptiveQuality(w, h)                    // qualité calculée en fonction de la surface en pixels

	buf := bufPool.Get().(*bytes.Buffer) // type assertion nécessaire car Pool retourne any
	buf.Reset()                          // vider sans réallouer — le buffer a peut-être servi pour une requête précédente
	logger.Debug().Str("step", "pool").Msg("buffer récupéré depuis sync.Pool")

	if err := jpeg.Encode(buf, img, &jpeg.Options{Quality: q}); err != nil {
		bufPool.Put(buf) // remettre le buffer même en cas d'erreur pour ne pas le perdre
		return nil, "", 0, err
	}
	return buf, "image/jpeg", q, nil
}

// adaptiveQuality choisit la qualité JPEG en fonction du nombre de pixels de l'image de sortie.
// Plus l'image est grande, plus elle mérite une qualité élevée pour préserver les détails.
func adaptiveQuality(w, h int) int {
	pixels := w * h // surface totale — critère plus pertinent que la largeur seule
	switch {
	case pixels < 500*500:   // miniature (< 250K pixels) — la compression artefact est moins visible
		return 80
	case pixels < 1920*1080: // HD (< 2M pixels)
		return 85
	default: // Full HD et au-delà — chaque pixel compte davantage
		return 90
	}
}

// ── Watermark ─────────────────────────────────────────────────────────────────

// applyWatermark dessine le texte sur une copie RGBA de l'image source.
// La couleur du texte est choisie dynamiquement en fonction de la luminosité
// du fond à l'endroit où sera positionné le watermark.
func applyWatermark(img image.Image, text, position string) (image.Image, error) {
	canvas := image.NewRGBA(img.Bounds())                           // copie RGBA pour rendre l'image modifiable (img source peut être read-only)
	draw.Draw(canvas, canvas.Bounds(), img, image.Point{}, draw.Src) // copier les pixels source sur le canvas avant de dessiner par-dessus

	textWidth := font.MeasureString(fontFace, text).Ceil()                                         // largeur en pixels pour positionner le texte à droite sans déborder
	wmX, wmY := wmCoords(textWidth, canvas.Bounds().Max.X, canvas.Bounds().Max.Y, position)        // coordonnées du coin bas-gauche du texte
	wmColor := adaptiveColor(img, wmX, wmY)                                                        // blanc ou gris foncé selon la luminosité du fond

	d := &font.Drawer{
		Dst:  canvas,
		Src:  image.NewUniform(wmColor), // couleur uniforme sur toute la surface du texte
		Face: fontFace,
		// Dot est la baseline du texte (coin bas-gauche du premier glyphe).
		Dot: fixed.Point26_6{
			X: fixed.I(wmX), // fixed.I convertit un entier en fixed-point 26.6 (format requis par x/image/font)
			Y: fixed.I(wmY),
		},
	}
	d.DrawString(text) // rasterise le texte sur le canvas

	return canvas, nil
}

// wmCoords calcule les coordonnées (x, y) du point d'ancrage du watermark
// en fonction de la position demandée et des dimensions de l'image.
// (x, y) correspond à la baseline bas-gauche du texte dans le repère font.Drawer.
func wmCoords(textWidth, w, h int, position string) (x, y int) {
	switch position {
	case "top-left":
		return wmMargin, wmLineHeight + wmMargin // wmLineHeight décale vers le bas pour que le texte ne soit pas coupé en haut
	case "top-right":
		return w - textWidth - wmMargin, wmLineHeight + wmMargin // symétrique top-left, ancré à droite
	case "bottom-left":
		return wmMargin, h - wmMargin // h - margin = juste au-dessus du bord bas
	default: // bottom-right
		return w - textWidth - wmMargin, h - wmMargin // position par défaut — la moins intrusive pour les photos
	}
}

// ── Couleur adaptative ────────────────────────────────────────────────────────

// adaptiveColor choisit blanc ou gris foncé selon la luminosité moyenne du fond
// à l'endroit où sera tracé le watermark, afin de garantir la lisibilité
// sur n'importe quelle image (claire ou sombre).
func adaptiveColor(img image.Image, x, y int) color.RGBA {
	avg := sampleLuminance(img, x, y) // luminance moyenne de la zone où le watermark sera dessiné
	darkBg := avg <= 128              // seuil mi-chemin entre noir (0) et blanc (255)

	// En dessous : fond sombre → texte blanc. Au-dessus : fond clair → texte sombre.
	logger.Debug().Str("step", "adaptive_color").Float64("luminance", avg).Bool("dark_bg", darkBg).Msg("couleur adaptative")

	if darkBg {
		return color.RGBA{R: 255, G: 255, B: 255, A: 210} // blanc semi-transparent sur fond sombre
	}
	return color.RGBA{R: 30, G: 30, B: 30, A: 210} // gris foncé semi-transparent sur fond clair
}

// sampleLuminance calcule la luminance perceptuelle moyenne d'une zone de sampleW×sampleH px
// à partir du coin (x, y). Les bords sont clampés aux limites de l'image.
//
// Parallélisation : les lignes sont découpées en numCPU chunks, chaque goroutine écrit
// dans son index de totals[i] — sans mutex, sans false sharing (indices indépendants).
// Fallback séquentiel si rows < numCPU (overhead goroutine > gain).
//
// Formule ITU-R BT.601 : L = 0.299·R + 0.587·G + 0.114·B
// Les coefficients reflètent la sensibilité de l'œil humain : vert > rouge > bleu.
func sampleLuminance(img image.Image, x, y int) float64 {
	bounds := img.Bounds() // limites de l'image pour clamper la zone d'échantillonnage

	startX := x
	startY := max(y-sampleH, bounds.Min.Y) // on remonte de sampleH pixels au-dessus de la baseline du texte
	endX := min(startX+sampleW, bounds.Max.X) // clamp à droite — évite de lire hors de l'image
	endY := min(startY+sampleH, bounds.Max.Y) // clamp en bas

	rows := endY - startY // nombre réel de lignes après clamp (peut être < sampleH aux bords de l'image)
	cols := endX - startX
	if rows == 0 || cols == 0 { // zone vide si le watermark est positionné hors image
		return 0
	}

	numWorkers := runtime.NumCPU() // autant de workers que de cœurs — cohérent avec le sémaphore global

	// Sous ce seuil l'overhead de création des goroutines dépasse le gain de parallélisme.
	if rows < numWorkers {
		var total float64
		for py := startY; py < endY; py++ {
			for px := startX; px < endX; px++ {
				r, g, b, _ := img.At(px, py).RGBA()                                    // RGBA retourne des valeurs 16 bits (0-65535)
				total += 0.299*float64(r>>8) + 0.587*float64(g>>8) + 0.114*float64(b>>8) // >>8 ramène en 8 bits (0-255)
			}
		}
		return total / float64(rows*cols) // moyenne sur tous les pixels de la zone
	}

	// Chaque worker somme ses lignes dans totals[i] — pas de contention, pas de mutex.
	totals := make([]float64, numWorkers)                  // un accumulateur par worker — indices distincts → lock-free
	chunkSize := (rows + numWorkers - 1) / numWorkers // division ceiling pour que le dernier chunk couvre toutes les lignes

	var wg sync.WaitGroup
	for i := 0; i < numWorkers; i++ {
		rowStart := startY + i*chunkSize          // début de la tranche de lignes pour ce worker
		rowEnd := min(rowStart+chunkSize, endY)   // fin clampée — le dernier chunk peut être plus court
		if rowStart >= endY {                     // arrive si rows < numWorkers (déjà géré, mais gardé en sécurité)
			break
		}
		wg.Add(1)
		go func(rStart, rEnd, idx int) { // bornes passées par valeur pour éviter la capture par référence dans la boucle
			defer wg.Done()
			var t float64
			for py := rStart; py < rEnd; py++ {
				for px := startX; px < endX; px++ {
					r, g, b, _ := img.At(px, py).RGBA()                                  // RGBA retourne des valeurs 16 bits (0-65535)
					t += 0.299*float64(r>>8) + 0.587*float64(g>>8) + 0.114*float64(b>>8) // >>8 ramène en 8 bits (0-255)
				}
			}
			totals[idx] = t // écriture dans l'index exclusif du worker — aucune autre goroutine ne touche cet index
		}(rowStart, rowEnd, i)
	}
	wg.Wait() // attendre que tous les workers aient terminé avant d'agréger

	var total float64
	for _, t := range totals { // sommation séquentielle des sous-totaux — rapide car numWorkers entrées max
		total += t
	}
	return total / float64(rows*cols) // moyenne sur tous les pixels de la zone
}

// ── Resize ────────────────────────────────────────────────────────────────────

// resize redimensionne l'image si elle dépasse maxWidth×maxHeight,
// en préservant le ratio. L'interpolation BiLinear offre un bon compromis
// entre qualité visuelle et vitesse (meilleur que NearestNeighbor, moins coûteux que CatmullRom).
func resize(img image.Image) image.Image {
	w := img.Bounds().Dx() // largeur source
	h := img.Bounds().Dy() // hauteur source

	if w <= maxWidth && h <= maxHeight { // déjà dans les limites — retourner l'original évite une copie inutile
		return img
	}

	ratio := float64(w) / float64(h) // ratio à préserver pour ne pas déformer l'image
	newW, newH := maxWidth, maxHeight // cibles initiales — l'une sera réduite pour respecter le ratio
	if float64(maxWidth)/float64(maxHeight) > ratio { // l'image est plus "portrait" que la cible
		newW = int(float64(maxHeight) * ratio) // contrainte hauteur — réduire la largeur
	} else {
		newH = int(float64(maxWidth) / ratio) // contrainte largeur — réduire la hauteur
	}

	dst := image.NewRGBA(image.Rect(0, 0, newW, newH))                              // canvas destination aux nouvelles dimensions
	xdraw.BiLinear.Scale(dst, dst.Bounds(), img, img.Bounds(), xdraw.Over, nil) // BiLinear : meilleur compromis qualité/vitesse pour le redimensionnement
	return dst
}

// ── Font ──────────────────────────────────────────────────────────────────────

// loadFont charge la police Go Regular embarquée dans le binaire et crée le font.Face global.
// La police est compilée dans l'exécutable via goregular.TTF — aucun fichier externe requis,
// ce qui simplifie le déploiement Docker (pas de dépendance apk ou de montage de volume).
func loadFont() error {
	t := time.Now()
	fontBytes := goregular.TTF // police embarquée — zéro I/O disque au démarrage

	f, err := opentype.Parse(fontBytes) // .ttf simple (pas une collection) → Parse suffit
	if err != nil {
		return err
	}

	// Taille 48pt @ 72 DPI = 48px — visible sur des images jusqu'à 1920px de large.
	fontFace, err = opentype.NewFace(f, &opentype.FaceOptions{
		Size: 48, // 48pt — visible sans écraser le sujet de la photo
		DPI:  72, // 72 DPI = convention écran (1pt = 1px)
	})

	logger.Info().Str("component", "init").Str("path", "embedded:go-regular").Str("size", formatBytes(len(fontBytes))).Dur("duration", time.Since(t)).Msg("police chargée")
	return err
}

// ── Utilitaires ───────────────────────────────────────────────────────────────

func formatBytes(b int) string {
	if b < 1024 { // en dessous d'un Ko — afficher en octets bruts
		return fmt.Sprintf("%d B", b)
	} else if b < 1024*1024 { // entre 1 Ko et 1 Mo
		return fmt.Sprintf("%.1f KB", float64(b)/1024)
	}
	return fmt.Sprintf("%.1f MB", float64(b)/1024/1024) // 1 Mo et plus
}
