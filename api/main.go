package main

import (
	"bytes"
	"compress/gzip" // compression gzip à la volée pour réduire la bande passante
	"fmt"
	"io"
	"mime/multipart" // construction du formulaire multipart envoyé à l'optimizer
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/rs/zerolog"
)

// Ce microservice reçoit une image, la forward à l'optimizer, puis renvoie le résultat au client.
var httpClient = &http.Client{Timeout: 30 * time.Second} // timeout global pour éviter de bloquer indéfiniment sur l'optimizer

var logger zerolog.Logger

// ── Main ─────────────────────────────────────────────────────────────────────

func main() {
	zerolog.TimeFieldFormat = time.RFC3339                                             // RFC3339 est plus lisible que l'epoch dans les logs structurés
	logger = zerolog.New(os.Stdout).With().Timestamp().Str("service", "api").Logger() // champ "service" identifie ce service dans une stack multi-conteneurs

	logger.Info().Str("addr", ":4000").Msg("démarrage")

	mux := http.NewServeMux()
	mux.HandleFunc("POST /upload", handleUpload) // point d'entrée principal : upload + watermark

	http.ListenAndServe(":4000", corsMiddleware(mux)) //nolint:errcheck — erreur fatale, le conteneur redémarre
}

// ── Handler ───────────────────────────────────────────────────────────────────

func handleUpload(w http.ResponseWriter, r *http.Request) {
	start := time.Now() // point de référence pour mesurer la durée totale du pipeline

	// ── ① Lecture ────────────────────────────────────────
	file, header, err := r.FormFile("image") // lit le fichier depuis le formulaire multipart
	if err != nil {
		http.Error(w, "Image manquante", http.StatusBadRequest)
		return
	}
	defer file.Close() // libérer la mémoire multipart dès que le handler retourne

	tRead := time.Now()
	data, err := io.ReadAll(file) // charger l'image en mémoire — nécessaire pour envoyer à l'optimizer
	if err != nil {
		http.Error(w, "Erreur lecture", http.StatusInternalServerError)
		return
	}
	readDur := time.Since(tRead)
	logger.Info().Str("step", "read").Str("filename", header.Filename).Str("size", formatBytes(len(data))).Dur("duration", readDur).Msg("lecture image")

	// ── ② Paramètres watermark + format de sortie ────────
	wmText := r.FormValue("wm_text")
	if wmText == "" {
		wmText = "NWS © 2026" // fallback si le champ est absent (appel direct à l'API)
	}
	wmPosition := r.FormValue("wm_position")
	if wmPosition == "" {
		wmPosition = "bottom-right" // position la moins intrusive par défaut
	}
	// Négociation de format : WebP si le navigateur le supporte (~30% plus léger), JPEG sinon.
	wmFormat := bestFormat(r)
	logger.Info().Str("step", "format").Str("accept", r.Header.Get("Accept")).Str("chosen", wmFormat).Msg("négociation format")

	// ── ③ Forward vers l'optimizer ───────────────────────
	optimizerURL := os.Getenv("OPTIMIZER_URL")
	if optimizerURL == "" {
		optimizerURL = "http://localhost:3001" // défaut dev local
	}

	tOptimizer := time.Now()
	result, err := sendToOptimizer(optimizerURL, header.Filename, data, wmText, wmPosition, wmFormat)
	if err != nil {
		logger.Error().Str("step", "optimizer").Err(err).Msg("optimizer KO")
		http.Error(w, "Microservice indisponible", http.StatusBadGateway)
		return
	}
	optimizerDur := time.Since(tOptimizer)
	logger.Info().Str("step", "optimizer").Str("format", wmFormat).Str("size", formatBytes(len(result))).Dur("duration", optimizerDur).Msg("image optimisée")

	// ── ④ Réponse ─────────────────────────────────────────
	gzipped := strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") // loggé pour debug — la compression est gérée dans sendResponse
	logger.Info().Str("step", "response").Bool("gzip", gzipped).Str("format", wmFormat).Str("size", formatBytes(len(result))).Msg("envoi réponse")
	logger.Info().Str("step", "total").Dur("duration", time.Since(start)).Msg("requête terminée")

	w.Header().Set("X-T-Read", fmtMs(readDur))
	w.Header().Set("X-T-Optimizer", fmtMs(optimizerDur))
	w.Header().Set("Vary", "Accept") // indique au CDN que la réponse varie selon le header Accept
	sendResponse(w, r, result)
}

// ── Helpers ───────────────────────────────────────────────────────────────────

// bestFormat lit le header Accept et retourne "webp" ou "jpeg".
// WebP offre ~30% de réduction par rapport à JPEG à qualité visuelle équivalente.
func bestFormat(r *http.Request) string {
	if strings.Contains(r.Header.Get("Accept"), "image/webp") { // tous les navigateurs modernes supportent WebP
		return "webp"
	}
	return "jpeg" // fallback universel — Safari < 14, vieux IE, clients non-browser
}

// detectContentType identifie le format à partir des magic bytes.
// Utilisé pour fixer le Content-Type correct sans avoir besoin de le stocker séparément.
//
// Magic bytes : WebP = "RIFF????WEBP" | JPEG = 0xFF 0xD8
func detectContentType(data []byte) string {
	if len(data) >= 12 &&
		data[0] == 'R' && data[1] == 'I' && data[2] == 'F' && data[3] == 'F' && // signature RIFF (conteneur WebP)
		data[8] == 'W' && data[9] == 'E' && data[10] == 'B' && data[11] == 'P' { // identifiant WebP dans le conteneur RIFF
		return "image/webp"
	}
	return "image/jpeg" // tout ce qui n'est pas WebP est traité comme JPEG — on ne supporte que ces deux formats
}

// sendToOptimizer envoie l'image à l'optimizer via HTTP multipart et retourne le résultat.
// Utilise io.Pipe pour streamer le multipart sans charger deux fois l'image en mémoire.
func sendToOptimizer(optimizerURL, filename string, data []byte, wmText, wmPosition, wmFormat string) ([]byte, error) {
	pr, pw := io.Pipe()           // tuyau synchrone : la goroutine écrit pendant que Post lit
	mw := multipart.NewWriter(pw)

	go func() {
		part, err := mw.CreateFormFile("image", filename) // crée le champ multipart "image"
		if err != nil {
			pw.CloseWithError(err) // propage l'erreur au Post pour éviter un goroutine leak
			return
		}
		io.Copy(part, bytes.NewReader(data)) //nolint:errcheck — si la copie échoue, CloseWithError est géré par le Post
		mw.WriteField("wm_text", wmText)
		mw.WriteField("wm_position", wmPosition)
		mw.WriteField("wm_format", wmFormat)
		mw.Close() // finalise le boundary multipart
		pw.Close() // signale la fin du stream au lecteur (httpClient.Post)
	}()

	resp, err := httpClient.Post(optimizerURL+"/optimize", mw.FormDataContentType(), pr) // lit le pipe pendant que la goroutine écrit
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	return io.ReadAll(resp.Body) // lire la réponse complète (image encodée)
}

// sendResponse envoie les données au client avec le Content-Type correct (détecté par magic bytes)
// et compression gzip si le navigateur le supporte.
func sendResponse(w http.ResponseWriter, r *http.Request, data []byte) {
	ct := detectContentType(data)
	w.Header().Set("Content-Type", ct)

	if strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") { // le client supporte gzip → compresser à la volée
		w.Header().Set("Content-Encoding", "gzip")
		gz, err := gzip.NewWriterLevel(w, gzip.BestSpeed) // BestSpeed : favorise la latence sur le taux de compression
		if err != nil {
			http.Error(w, "Erreur compression", http.StatusInternalServerError)
			return
		}
		defer gz.Close()  // flush + écriture du footer gzip avant de retourner
		gz.Write(data)    //nolint:errcheck — erreur réseau côté client, pas récupérable
	} else {
		w.Write(data) //nolint:errcheck — erreur réseau côté client, pas récupérable
	}
}

// corsMiddleware ajoute les headers CORS pour permettre les appels depuis le front React (dev + prod).
func corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")                   // en prod, restreindre au domaine du front
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
		w.Header().Set("Access-Control-Expose-Headers", "X-T-Read, X-T-Optimizer") // expose les headers de timing au front pour le debug

		if r.Method == http.MethodOptions { // preflight CORS — répondre sans passer au handler
			w.WriteHeader(http.StatusNoContent)
			return
		}

		next.ServeHTTP(w, r)
	})
}

// fmtMs convertit une durée en millisecondes avec 3 décimales (ex: "12.345").
// Utilisé pour les headers X-T-* exposés au front pour le debug de performances.
func fmtMs(d time.Duration) string {
	return fmt.Sprintf("%.3f", float64(d.Microseconds())/1000) // Microseconds() évite la perte de précision de Milliseconds()
}

func formatBytes(b int) string {
	if b < 1024 { // en dessous d'un Ko — afficher en octets bruts
		return fmt.Sprintf("%d B", b)
	} else if b < 1024*1024 { // entre 1 Ko et 1 Mo
		return fmt.Sprintf("%.1f KB", float64(b)/1024)
	}
	return fmt.Sprintf("%.1f MB", float64(b)/1024/1024) // 1 Mo et plus
}