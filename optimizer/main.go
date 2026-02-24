package main

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/jpeg"
	_ "image/png"
	"net/http"
	"os"
	"runtime"
	"sync"
	"time"

	"github.com/rs/zerolog"
	xdraw "golang.org/x/image/draw"
	"golang.org/x/image/font"
	"golang.org/x/image/font/opentype"
	"golang.org/x/image/math/fixed"
)

const (
	maxWidth  = 1920 // largeur maximale après resize
	maxHeight = 1080 // hauteur maximale après resize
	quality   = 85   // qualité JPEG (bon compromis taille / qualité visuelle)

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

// bufPool réutilise les buffers JPEG entre les requêtes pour réduire la pression GC.
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

	// ── ② Décodage ───────────────────────────────────────
	t := time.Now()
	img, format, err := decodeImage(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	origW, origH := img.Bounds().Dx(), img.Bounds().Dy()
	logger.Info().Str("step", "decode").Str("format", format).Int("width", origW).Int("height", origH).Dur("duration", time.Since(t)).Msg("décodage")

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
	wmText, wmPosition := wmParams(r)
	watermarked, err := applyWatermark(resized, wmText, wmPosition)
	if err != nil {
		http.Error(w, "Erreur watermark", http.StatusInternalServerError)
		return
	}
	logger.Info().Str("step", "watermark").Str("text", wmText).Str("position", wmPosition).Dur("duration", time.Since(t)).Msg("watermark appliqué")

	// ── ⑤ Encodage JPEG ──────────────────────────────────
	t = time.Now()
	buf, err := encodeToBuffer(watermarked)
	if err != nil {
		http.Error(w, "Erreur encodage", http.StatusInternalServerError)
		return
	}
	defer bufPool.Put(buf)
	logger.Info().Str("step", "encode").Int("quality", quality).Str("size", formatBytes(buf.Len())).Dur("duration", time.Since(t)).Msg("encodage JPEG")
	logger.Info().Str("step", "total").Dur("duration", time.Since(start)).Msg("image traitée")

	w.Header().Set("Content-Type", "image/jpeg")
	w.Write(buf.Bytes())
}

// ── Pipeline steps ────────────────────────────────────────────────────────────

// decodeImage lit le fichier multipart "image" et le décode en image.Image.
// On supporte JPEG et PNG (le blank import de image/png enregistre le décodeur).
func decodeImage(r *http.Request) (image.Image, string, error) {
	file, _, err := r.FormFile("image")
	if err != nil {
		return nil, "", fmt.Errorf("image manquante")
	}
	defer file.Close()

	img, format, err := image.Decode(file)
	if err != nil {
		return nil, "", fmt.Errorf("format invalide")
	}
	return img, format, nil
}

// wmParams lit les paramètres de watermark depuis le formulaire multipart.
// Les valeurs par défaut garantissent un comportement cohérent même si le front
// n'envoie pas ces champs (appels directs à l'API, retry RabbitMQ, etc.).
func wmParams(r *http.Request) (text, position string) {
	text = r.FormValue("wm_text")
	if text == "" {
		text = "NWS © 2026"
	}
	position = r.FormValue("wm_position")
	if position == "" {
		position = "bottom-right"
	}
	return
}

// encodeToBuffer encode l'image en JPEG dans un buffer recyclé depuis le sync.Pool.
// Le caller est responsable de remettre le buffer dans le pool (defer bufPool.Put(buf)).
func encodeToBuffer(img image.Image) (*bytes.Buffer, error) {
	buf := bufPool.Get().(*bytes.Buffer)
	buf.Reset()
	logger.Debug().Str("step", "pool").Msg("buffer récupéré depuis sync.Pool")
	if err := jpeg.Encode(buf, img, &jpeg.Options{Quality: quality}); err != nil {
		bufPool.Put(buf) // on remet le buffer même en cas d'erreur
		return nil, err
	}
	return buf, nil
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
// Formule ITU-R BT.601 : L = 0.299·R + 0.587·G + 0.114·B
// Les coefficients reflètent la sensibilité de l'œil humain : vert > rouge > bleu.
func sampleLuminance(img image.Image, x, y int) float64 {
	bounds := img.Bounds()

	// Calcul de la zone d'échantillonnage, clampée aux bords de l'image.
	startX := x
	startY := max(y-sampleH, bounds.Min.Y)
	endX := min(startX+sampleW, bounds.Max.X)
	endY := min(startY+sampleH, bounds.Max.Y)

	var total float64
	var count int

	for py := startY; py < endY; py++ {
		for px := startX; px < endX; px++ {
			// RGBA() retourne des valeurs uint32 dans [0, 65535] ; >>8 ramène à [0, 255].
			r, g, b, _ := img.At(px, py).RGBA()
			total += 0.299*float64(r>>8) + 0.587*float64(g>>8) + 0.114*float64(b>>8)
			count++
		}
	}

	if count == 0 {
		return 0
	}
	return total / float64(count)
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
