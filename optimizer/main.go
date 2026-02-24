package main

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/jpeg"
	_ "image/png"
	"io"
	"net/http"
	"os"
	"runtime"
	"sync"
	"time"

	"github.com/chai2010/webp"
	"github.com/rs/zerolog"
	xdraw "golang.org/x/image/draw"
	"golang.org/x/image/font"
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
	zerolog.TimeFieldFormat = time.RFC3339
	logger = zerolog.New(os.Stdout).With().Timestamp().Str("service", "optimizer").Logger()

	numCPU := runtime.NumCPU()
	logger.Info().Str("addr", ":3001").Int("worker_slots", numCPU).Msg("démarrage")

	if err := loadFont(); err != nil {
		logger.Fatal().Err(err).Msg("chargement police échoué")
	}

	mux := http.NewServeMux()
	mux.HandleFunc("POST /optimize", handleOptimize)

	http.ListenAndServe(":3001", mux)
}

// ── Handler ───────────────────────────────────────────────────────────────────

func handleOptimize(w http.ResponseWriter, r *http.Request) {
	start := time.Now()

	// ── ① Worker Pool ────────────────────────────────────
	// On log avant d'acquérir le slot pour tracer les pics de charge.
	slotsUsed := len(sem) + 1
	totalSlots := cap(sem)
	logger.Info().Str("step", "worker_pool").Int("used", slotsUsed).Int("total", totalSlots).Msg("slot acquis")

	sem <- struct{}{}
	defer func() {
		<-sem
		logger.Info().Str("step", "worker_pool").Int("used", len(sem)).Int("total", totalSlots).Msg("slot libéré")
	}()

	// ── ② Décodage (lazy validation + full decode) ────────
	t := time.Now()
	img, format, err := decodeImage(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	origW, origH := img.Bounds().Dx(), img.Bounds().Dy()
	logger.Info().Str("step", "decode").Str("format", format).Int("width", origW).Int("height", origH).Dur("duration", time.Since(t)).Msg("décodage + strip EXIF")

	// ── ③ Resize ─────────────────────────────────────────
	t = time.Now()
	resized := resize(img)
	newW, newH := resized.Bounds().Dx(), resized.Bounds().Dy()
	if origW == newW && origH == newH {
		logger.Info().Str("step", "resize").Bool("resized", false).Int("max_w", maxWidth).Int("max_h", maxHeight).Msg("resize ignoré")
	} else {
		logger.Info().Str("step", "resize").Bool("resized", true).Int("from_w", origW).Int("from_h", origH).Int("to_w", newW).Int("to_h", newH).Dur("duration", time.Since(t)).Msg("resize")
	}

	// ── ④ Watermark ──────────────────────────────────────
	t = time.Now()
	wmText, wmPosition, wmFormat := wmParams(r)
	watermarked, err := applyWatermark(resized, wmText, wmPosition)
	if err != nil {
		http.Error(w, "Erreur watermark", http.StatusInternalServerError)
		return
	}
	logger.Info().Str("step", "watermark").Str("text", wmText).Str("position", wmPosition).Dur("duration", time.Since(t)).Msg("watermark appliqué")

	// ── ⑤ Encodage ────────────────────────────────────────
	t = time.Now()
	buf, contentType, q, err := encodeToBuffer(watermarked, wmFormat)
	if err != nil {
		http.Error(w, "Erreur encodage", http.StatusInternalServerError)
		return
	}
	defer bufPool.Put(buf)
	logger.Info().Str("step", "encode").Str("format", wmFormat).Int("quality", q).Str("size", formatBytes(buf.Len())).Dur("duration", time.Since(t)).Msg("encodage")
	logger.Info().Str("step", "total").Dur("duration", time.Since(start)).Msg("image traitée")

	w.Header().Set("Content-Type", contentType)
	w.Write(buf.Bytes())
}

// ── Pipeline steps ────────────────────────────────────────────────────────────

// decodeImage valide les dimensions via DecodeConfig (sans décoder les pixels),
// puis effectue le décodage complet. Le ré-encodage ultérieur supprime automatiquement
// les métadonnées EXIF (GPS, miniature, profil ICC) — gain de 5-15% sur les photos iPhone.
func decodeImage(r *http.Request) (image.Image, string, error) {
	file, _, err := r.FormFile("image")
	if err != nil {
		return nil, "", fmt.Errorf("image manquante")
	}
	defer file.Close()

	// ① Lazy decode : lit uniquement le header (quelques Ko) pour valider les dimensions
	// sans décompresser les ~25 millions de pixels d'une image 4K.
	config, format, err := image.DecodeConfig(file)
	if err != nil {
		return nil, "", fmt.Errorf("format invalide")
	}
	if config.Width > maxInputWidth || config.Height > maxInputHeight {
		return nil, "", fmt.Errorf("image trop grande (max %dx%d, reçu %dx%d)", maxInputWidth, maxInputHeight, config.Width, config.Height)
	}
	logger.Debug().Str("step", "lazy_decode").Str("format", format).Int("width", config.Width).Int("height", config.Height).Msg("dimensions validées sans décodage pixels")

	// ② Seek back to start before full decode — DecodeConfig a consommé le reader.
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return nil, "", fmt.Errorf("seek échoué")
	}

	img, _, err := image.Decode(file)
	if err != nil {
		return nil, "", fmt.Errorf("décodage échoué")
	}
	return img, format, nil
}

// wmParams lit les paramètres de watermark depuis le formulaire multipart.
// Les valeurs par défaut garantissent un comportement cohérent même si le front
// n'envoie pas ces champs (appels directs à l'API, retry RabbitMQ, etc.).
func wmParams(r *http.Request) (text, position, format string) {
	text = r.FormValue("wm_text")
	if text == "" {
		text = "NWS © 2026"
	}
	position = r.FormValue("wm_position")
	if position == "" {
		position = "bottom-right"
	}
	format = r.FormValue("wm_format")
	if format != "webp" {
		format = "jpeg" // seuls jpeg et webp sont supportés
	}
	return
}

// encodeToBuffer encode l'image dans un buffer recyclé depuis le sync.Pool.
// La qualité est adaptée dynamiquement aux dimensions de l'image de sortie.
// Retourne le buffer, le content-type et la qualité utilisée (pour le log).
// Le caller est responsable de remettre le buffer dans le pool (defer bufPool.Put(buf)).
func encodeToBuffer(img image.Image, format string) (*bytes.Buffer, string, int, error) {
	w, h := img.Bounds().Dx(), img.Bounds().Dy()
	q := adaptiveQuality(w, h)

	buf := bufPool.Get().(*bytes.Buffer)
	buf.Reset()
	logger.Debug().Str("step", "pool").Msg("buffer récupéré depuis sync.Pool")

	switch format {
	case "webp":
		// WebP qualité : environ 5 points en dessous de JPEG pour une qualité perçue équivalente.
		// Ex: JPEG 85 ≈ WebP 80 — le codec WebP est plus efficace à même valeur numérique.
		wq := float32(q - 5)
		if err := webp.Encode(buf, img, &webp.Options{Lossless: false, Quality: wq}); err != nil {
			bufPool.Put(buf)
			return nil, "", 0, err
		}
		return buf, "image/webp", int(wq), nil

	default: // jpeg
		if err := jpeg.Encode(buf, img, &jpeg.Options{Quality: q}); err != nil {
			bufPool.Put(buf)
			return nil, "", 0, err
		}
		return buf, "image/jpeg", q, nil
	}
}

// adaptiveQuality choisit la qualité JPEG en fonction du nombre de pixels de l'image de sortie.
// Plus l'image est grande, plus elle mérite une qualité élevée pour préserver les détails.
func adaptiveQuality(w, h int) int {
	pixels := w * h
	switch {
	case pixels < 500*500:   // miniature (< 250K pixels)
		return 80
	case pixels < 1920*1080: // HD (< 2M pixels)
		return 85
	default: // Full HD et au-delà
		return 90
	}
}

// ── Watermark ─────────────────────────────────────────────────────────────────

// applyWatermark dessine le texte sur une copie RGBA de l'image source.
// La couleur du texte est choisie dynamiquement en fonction de la luminosité
// du fond à l'endroit où sera positionné le watermark.
func applyWatermark(img image.Image, text, position string) (image.Image, error) {
	// On copie l'image dans un canvas RGBA pour pouvoir y dessiner par-dessus.
	canvas := image.NewRGBA(img.Bounds())
	draw.Draw(canvas, canvas.Bounds(), img, image.Point{}, draw.Src)

	textWidth := font.MeasureString(fontFace, text).Ceil()
	wmX, wmY := wmCoords(textWidth, canvas.Bounds().Max.X, canvas.Bounds().Max.Y, position)
	wmColor := adaptiveColor(img, wmX, wmY)

	d := &font.Drawer{
		Dst:  canvas,
		Src:  image.NewUniform(wmColor),
		Face: fontFace,
		// Dot est la baseline du texte (coin bas-gauche du premier glyphe).
		Dot: fixed.Point26_6{
			X: fixed.I(wmX),
			Y: fixed.I(wmY),
		},
	}
	d.DrawString(text)

	return canvas, nil
}

// wmCoords calcule les coordonnées (x, y) du point d'ancrage du watermark
// en fonction de la position demandée et des dimensions de l'image.
// (x, y) correspond à la baseline bas-gauche du texte dans le repère font.Drawer.
func wmCoords(textWidth, w, h int, position string) (x, y int) {
	switch position {
	case "top-left":
		return wmMargin, wmLineHeight + wmMargin
	case "top-right":
		return w - textWidth - wmMargin, wmLineHeight + wmMargin
	case "bottom-left":
		return wmMargin, h - wmMargin
	default: // bottom-right
		return w - textWidth - wmMargin, h - wmMargin
	}
}

// ── Couleur adaptative ────────────────────────────────────────────────────────

// adaptiveColor choisit blanc ou gris foncé selon la luminosité moyenne du fond
// à l'endroit où sera tracé le watermark, afin de garantir la lisibilité
// sur n'importe quelle image (claire ou sombre).
func adaptiveColor(img image.Image, x, y int) color.RGBA {
	avg := sampleLuminance(img, x, y)
	darkBg := avg <= 128

	// Seuil 128 = mi-chemin entre noir (0) et blanc (255).
	// En dessous : fond sombre → texte blanc. Au-dessus : fond clair → texte sombre.
	logger.Debug().Str("step", "adaptive_color").Float64("luminance", avg).Bool("dark_bg", darkBg).Msg("couleur adaptative")

	if darkBg {
		return color.RGBA{R: 255, G: 255, B: 255, A: 210}
	}
	return color.RGBA{R: 30, G: 30, B: 30, A: 210}
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
	bounds := img.Bounds()

	startX := x
	startY := max(y-sampleH, bounds.Min.Y)
	endX := min(startX+sampleW, bounds.Max.X)
	endY := min(startY+sampleH, bounds.Max.Y)

	rows := endY - startY
	cols := endX - startX
	if rows == 0 || cols == 0 {
		return 0
	}

	numWorkers := runtime.NumCPU()

	// Sous ce seuil l'overhead de création des goroutines dépasse le gain de parallélisme.
	if rows < numWorkers {
		var total float64
		for py := startY; py < endY; py++ {
			for px := startX; px < endX; px++ {
				r, g, b, _ := img.At(px, py).RGBA()
				total += 0.299*float64(r>>8) + 0.587*float64(g>>8) + 0.114*float64(b>>8)
			}
		}
		return total / float64(rows*cols)
	}

	// Chaque worker somme ses lignes dans totals[i] — pas de contention, pas de mutex.
	totals := make([]float64, numWorkers)
	chunkSize := (rows + numWorkers - 1) / numWorkers

	var wg sync.WaitGroup
	for i := 0; i < numWorkers; i++ {
		rowStart := startY + i*chunkSize
		rowEnd := min(rowStart+chunkSize, endY)
		if rowStart >= endY {
			break
		}
		wg.Add(1)
		go func(rStart, rEnd, idx int) {
			defer wg.Done()
			var t float64
			for py := rStart; py < rEnd; py++ {
				for px := startX; px < endX; px++ {
					r, g, b, _ := img.At(px, py).RGBA()
					t += 0.299*float64(r>>8) + 0.587*float64(g>>8) + 0.114*float64(b>>8)
				}
			}
			totals[idx] = t
		}(rowStart, rowEnd, i)
	}
	wg.Wait()

	var total float64
	for _, t := range totals {
		total += t
	}
	return total / float64(rows*cols)
}

// ── Resize ────────────────────────────────────────────────────────────────────

// resize redimensionne l'image si elle dépasse maxWidth×maxHeight,
// en préservant le ratio. L'interpolation BiLinear offre un bon compromis
// entre qualité visuelle et vitesse (meilleur que NearestNeighbor, moins coûteux que CatmullRom).
func resize(img image.Image) image.Image {
	w := img.Bounds().Dx()
	h := img.Bounds().Dy()

	// Si l'image tient déjà dans les limites, on la retourne telle quelle
	// pour éviter une copie inutile.
	if w <= maxWidth && h <= maxHeight {
		return img
	}

	// On calcule les nouvelles dimensions en conservant le ratio d'aspect.
	// La contrainte la plus restrictive (largeur ou hauteur) détermine l'échelle.
	ratio := float64(w) / float64(h)
	newW, newH := maxWidth, maxHeight
	if float64(maxWidth)/float64(maxHeight) > ratio {
		newW = int(float64(maxHeight) * ratio)
	} else {
		newH = int(float64(maxWidth) / ratio)
	}

	dst := image.NewRGBA(image.Rect(0, 0, newW, newH))
	xdraw.BiLinear.Scale(dst, dst.Bounds(), img, img.Bounds(), xdraw.Over, nil)
	return dst
}

// ── Font ──────────────────────────────────────────────────────────────────────

// loadFont charge la police TTF depuis le disque et crée le font.Face global.
// Appelé une seule fois au démarrage : lire le disque à chaque requête serait trop coûteux.
// FONT_PATH permet de surcharger la police via variable d'environnement (utile en Docker).
func loadFont() error {
	fontPath := os.Getenv("FONT_PATH")
	if fontPath == "" {
		fontPath = "/System/Library/Fonts/Helvetica.ttc"
	}

	t := time.Now()
	fontBytes, err := os.ReadFile(fontPath)
	if err != nil {
		return err
	}

	// ParseCollection gère à la fois les fichiers .ttf (une seule fonte)
	// et les collections .ttc (plusieurs fontes) — on prend toujours la première.
	f, err := opentype.ParseCollection(fontBytes)
	if err != nil {
		return err
	}

	font0, err := f.Font(0)
	if err != nil {
		return err
	}

	// Taille 48pt @ 72 DPI = 48px — visible sur des images jusqu'à 1920px de large.
	fontFace, err = opentype.NewFace(font0, &opentype.FaceOptions{
		Size: 48,
		DPI:  72,
	})

	logger.Info().Str("component", "init").Str("path", fontPath).Str("size", formatBytes(len(fontBytes))).Dur("duration", time.Since(t)).Msg("police chargée")
	return err
}

// ── Utilitaires ───────────────────────────────────────────────────────────────

func formatBytes(b int) string {
	if b < 1024 {
		return fmt.Sprintf("%d B", b)
	} else if b < 1024*1024 {
		return fmt.Sprintf("%.1f KB", float64(b)/1024)
	}
	return fmt.Sprintf("%.1f MB", float64(b)/1024/1024)
}
