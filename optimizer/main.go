package main

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/jpeg"
	_ "image/png"
	"log"
	"net/http"
	"os"
	"runtime"
	"sync"
	"time"

	xdraw "golang.org/x/image/draw"
	"golang.org/x/image/font"
	"golang.org/x/image/font/opentype"
	"golang.org/x/image/math/fixed"
)

const (
	maxWidth  = 1920
	maxHeight = 1080
	quality   = 85
	wmText    = "NWS © 2026"
)

var sem = make(chan struct{}, runtime.NumCPU())
var bufPool = sync.Pool{
	New: func() any { return new(bytes.Buffer) },
}
var fontFace font.Face

func main() {
	numCPU := runtime.NumCPU()
	log.Printf("[OPTIMIZER] Démarrage sur :3001")
	log.Printf("[OPTIMIZER] ✓ Worker pool     : %d slots (= %d coeurs CPU détectés)", numCPU, numCPU)
	log.Printf("[OPTIMIZER] ✓ sync.Pool       : buffers réutilisables initialisés")

	if err := loadFont(); err != nil {
		log.Fatalf("[OPTIMIZER] ✗ Police : %v", err)
	}
	log.Printf("[OPTIMIZER] ✓ Police TTF      : chargée une fois en mémoire (pas de lecture disque par requête)")
	log.Println("[OPTIMIZER] ─────────────────────────────────────────")

	mux := http.NewServeMux()
	mux.HandleFunc("POST /optimize", handleOptimize)

	http.ListenAndServe(":3001", mux)
}

func handleOptimize(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	log.Println("[OPTIMIZER] ┌─ Nouvelle image reçue")

	// ── Worker Pool ───────────────────────────────────────
	slotsUsed := len(sem) + 1
	totalSlots := cap(sem)
	log.Printf("[OPTIMIZER] │ ① Worker pool  : slot %d/%d occupé", slotsUsed, totalSlots)

	// Acquiert un slot — bloque si tous les slots sont pris
	sem <- struct{}{}
	defer func() {
		<-sem // libère le slot
		log.Printf("[OPTIMIZER] │   Worker pool  : slot libéré (%d/%d utilisés)", len(sem), totalSlots)
	}()

	// ── Décodage ─────────────────────────────────────────
	t := time.Now()
	file, _, err := r.FormFile("image")
	if err != nil {
		http.Error(w, "Image manquante", http.StatusBadRequest)
		return
	}
	defer file.Close()

	img, format, err := image.Decode(file)
	if err != nil {
		http.Error(w, "Format invalide", http.StatusBadRequest)
		return
	}
	origW, origH := img.Bounds().Dx(), img.Bounds().Dy()
	log.Printf("[OPTIMIZER] │ ② Décodage     : format=%s | %dx%d | en %v", format, origW, origH, time.Since(t))

	// ── Resize ───────────────────────────────────────────
	t = time.Now()
	resized := resize(img)
	newW, newH := resized.Bounds().Dx(), resized.Bounds().Dy()
	if origW == newW && origH == newH {
		log.Printf("[OPTIMIZER] │ ③ Resize       : aucun (déjà dans les limites %dx%d)", maxWidth, maxHeight)
	} else {
		log.Printf("[OPTIMIZER] │ ③ Resize       : %dx%d → %dx%d | BiLinear | en %v", origW, origH, newW, newH, time.Since(t))
	}

	// ── Watermark ────────────────────────────────────────
	t = time.Now()
	watermarked, err := applyWatermark(resized)
	if err != nil {
		http.Error(w, "Erreur watermark", http.StatusInternalServerError)
		return
	}
	log.Printf("[OPTIMIZER] │ ④ Watermark    : \"%s\" appliqué | police en mémoire | en %v", wmText, time.Since(t))

	// ── sync.Pool ─────────────────────────────────────────
	t = time.Now()
	buf := bufPool.Get().(*bytes.Buffer)
	buf.Reset()
	defer bufPool.Put(buf)
	log.Printf("[OPTIMIZER] │ ⑤ sync.Pool    : buffer récupéré (recyclé, pas d'allocation)")

	// ── Encodage JPEG ─────────────────────────────────────
	if err := jpeg.Encode(buf, watermarked, &jpeg.Options{Quality: quality}); err != nil {
		http.Error(w, "Erreur encodage", http.StatusInternalServerError)
		return
	}
	log.Printf("[OPTIMIZER] │ ⑥ JPEG encode  : qualité=%d%% | %s | en %v", quality, formatBytes(buf.Len()), time.Since(t))
	log.Printf("[OPTIMIZER] └─ ⏱ Total       : %v", time.Since(start))

	w.Header().Set("Content-Type", "image/jpeg")
	w.Write(buf.Bytes())
}

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

	f, err := opentype.ParseCollection(fontBytes)
	if err != nil {
		return err
	}

	font0, err := f.Font(0)
	if err != nil {
		return err
	}

	fontFace, err = opentype.NewFace(font0, &opentype.FaceOptions{
		Size: 48,
		DPI:  72,
	})

	log.Printf("[OPTIMIZER] ✓ Police chargée en %v (%s)", time.Since(t), formatBytes(len(fontBytes)))
	return err
}

func applyWatermark(img image.Image) (image.Image, error) {
	canvas := image.NewRGBA(img.Bounds())
	draw.Draw(canvas, canvas.Bounds(), img, image.Point{}, draw.Src)

	wmX := 20
	wmY := canvas.Bounds().Max.Y - 40

	// Échantillonne la zone où sera dessiné le watermark (200x50px en bas à gauche)
	// pour calculer la luminosité moyenne du fond.
	wmColor := adaptiveColor(img, wmX, wmY)

	d := &font.Drawer{
		Dst:  canvas,
		Src:  image.NewUniform(wmColor),
		Face: fontFace,
		Dot: fixed.Point26_6{
			X: fixed.I(wmX),
			Y: fixed.I(wmY),
		},
	}
	d.DrawString(wmText)

	return canvas, nil
}

// adaptiveColor analyse la luminosité de la zone où sera positionné le watermark.
// Luminosité = formule perceptuelle : l'oeil humain est plus sensible au vert qu'au rouge/bleu.
// Si le fond est clair (luminosité > 128) → texte sombre.
// Si le fond est sombre (luminosité ≤ 128) → texte blanc.
func adaptiveColor(img image.Image, x, y int) color.RGBA {
	bounds := img.Bounds()

	// Zone d'échantillonnage : rectangle de 200x50px autour du watermark
	sampleW := 200
	sampleH := 50
	startX := x
	startY := y - sampleH
	if startY < bounds.Min.Y {
		startY = bounds.Min.Y
	}
	endX := startX + sampleW
	if endX > bounds.Max.X {
		endX = bounds.Max.X
	}
	endY := startY + sampleH
	if endY > bounds.Max.Y {
		endY = bounds.Max.Y
	}

	var totalLuminance float64
	var count int

	for py := startY; py < endY; py++ {
		for px := startX; px < endX; px++ {
			r, g, b, _ := img.At(px, py).RGBA()
			// Convertit de uint32 (0-65535) en float64 (0-255)
			// Formule perceptuelle ITU-R BT.601 : l'oeil voit mieux le vert
			luminance := 0.299*float64(r>>8) + 0.587*float64(g>>8) + 0.114*float64(b>>8)
			totalLuminance += luminance
			count++
		}
	}

	avgLuminance := totalLuminance / float64(count)

	log.Printf("[OPTIMIZER] │   Watermark    : luminosité zone=%.1f/255 → ", avgLuminance)

	// Fond clair → texte sombre | Fond sombre → texte blanc
	if avgLuminance > 128 {
		log.Printf("[OPTIMIZER] │                  fond CLAIR → texte sombre")
		return color.RGBA{R: 30, G: 30, B: 30, A: 210}
	}
	log.Printf("[OPTIMIZER] │                  fond SOMBRE → texte blanc")
	return color.RGBA{R: 255, G: 255, B: 255, A: 210}
}

func resize(img image.Image) image.Image {
	w := img.Bounds().Dx()
	h := img.Bounds().Dy()

	if w <= maxWidth && h <= maxHeight {
		return img
	}

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

func formatBytes(b int) string {
	if b < 1024 {
		return fmt.Sprintf("%d B", b)
	} else if b < 1024*1024 {
		return fmt.Sprintf("%.1f KB", float64(b)/1024)
	}
	return fmt.Sprintf("%.1f MB", float64(b)/1024/1024)
}
