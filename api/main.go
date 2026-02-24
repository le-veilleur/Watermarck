package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/rs/zerolog"
	amqp "github.com/rabbitmq/amqp091-go"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
	"github.com/redis/go-redis/v9"
)

// Ce microservice API reçoit une image, vérifie si une version optimisée existe déjà dans Redis (cache),
// sinon il la forward à l'optimizer, stocke le résultat dans Redis, puis renvoie l'image optimisée au client.
// Si l'optimizer est KO, le job est publié dans RabbitMQ pour être retraité (Option B : HTTP sync + fallback).
const minioBucket = "watermarks"

var httpClient = &http.Client{Timeout: 30 * time.Second}
var redisClient *redis.Client
var minioClient *minio.Client
var amqpChan *amqp.Channel

// logger est le logger structuré pour le handler HTTP.
// wlog est le logger du worker RabbitMQ (champ component=worker ajouté).
var (
	logger zerolog.Logger
	wlog   zerolog.Logger
)

// RetryJob représente un job publié dans RabbitMQ quand l'optimizer est KO.
type RetryJob struct {
	Hash        string `json:"hash"`
	OriginalKey string `json:"original_key"`
	Filename    string `json:"filename"`
	WmText      string `json:"wm_text"`
	WmPosition  string `json:"wm_position"`
	WmFormat    string `json:"wm_format"`
}

// ── Main ─────────────────────────────────────────────────────────────────────
func main() {
	zerolog.TimeFieldFormat = time.RFC3339
	logger = zerolog.New(os.Stdout).With().Timestamp().Str("service", "api").Logger()
	wlog = logger.With().Str("component", "worker").Logger()

	redisClient = initRedis()
	minioClient = initMinio()
	amqpChan = initRabbitMQ()

	go retryWorker()

	logger.Info().Str("addr", ":3000").Msg("démarrage")

	mux := http.NewServeMux()
	mux.HandleFunc("POST /upload", handleUpload)
	mux.HandleFunc("GET /status/{hash}", handleStatus)
	mux.HandleFunc("GET /image/{hash}", handleGetImage)

	http.ListenAndServe(":3000", corsMiddleware(mux))
}

// ── Init ─────────────────────────────────────────────────────────────────────

func initRedis() *redis.Client {
	redisURL := os.Getenv("REDIS_URL")
	if redisURL == "" {
		redisURL = "redis://localhost:6379"
	}
	opt, err := redis.ParseURL(redisURL)
	if err != nil {
		logger.Fatal().Err(err).Msg("redis URL invalide")
	}
	client := redis.NewClient(opt)
	if err := client.Ping(context.Background()).Err(); err != nil {
		logger.Fatal().Err(err).Msg("impossible de se connecter à redis")
	}
	logger.Info().Str("component", "init").Msg("redis connecté")
	return client
}

func initMinio() *minio.Client {
	// On lit les infos de connexion à MinIO depuis les variables d'environnement, avec des valeurs par défaut pour le dev local.
	endpoint := os.Getenv("MINIO_ENDPOINT")
	if endpoint == "" {
		endpoint = "localhost:9000"
	}
	// Par défaut, MinIO utilise "minioadmin" comme user/password en local, mais on peut les override en prod.
	user := os.Getenv("MINIO_ROOT_USER")
	if user == "" {
		user = "minioadmin"
	}
	// Par défaut, MinIO utilise "minioadmin" comme user/password en local, mais on peut les override en prod.
	password := os.Getenv("MINIO_ROOT_PASSWORD")
	if password == "" {
		password = "minioadmin"
	}

	client, err := minio.New(endpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(user, password, ""),
		Secure: false,
	})
	if err != nil {
		logger.Fatal().Err(err).Msg("minio client invalide")
	}

	ctx := context.Background()
	exists, err := client.BucketExists(ctx, minioBucket)
	if err != nil {
		logger.Fatal().Err(err).Msg("minio inaccessible")
	}
	if !exists {
		if err := client.MakeBucket(ctx, minioBucket, minio.MakeBucketOptions{}); err != nil {
			logger.Fatal().Err(err).Str("bucket", minioBucket).Msg("impossible de créer le bucket")
		}
		logger.Info().Str("component", "init").Str("bucket", minioBucket).Msg("bucket minio créé")
	} else {
		logger.Info().Str("component", "init").Str("bucket", minioBucket).Msg("minio connecté")
	}
	return client
}

func initRabbitMQ() *amqp.Channel {
	// On lit l'URL de connexion à RabbitMQ depuis les variables d'environnement, avec une valeur par défaut pour le dev local.
	rabbitmqURL := os.Getenv("RABBITMQ_URL")
	if rabbitmqURL == "" {
		rabbitmqURL = "amqp://guest:guest@localhost:5672/"
	}
	conn, err := amqp.Dial(rabbitmqURL)
	if err != nil {
		logger.Fatal().Err(err).Msg("impossible de se connecter à rabbitmq")
	}
	ch, err := conn.Channel()
	if err != nil {
		logger.Fatal().Err(err).Msg("impossible d'ouvrir un channel rabbitmq")
	}
	_, err = ch.QueueDeclare("watermark_retry", true, false, false, false, nil)
	if err != nil {
		logger.Fatal().Err(err).Str("queue", "watermark_retry").Msg("impossible de déclarer la queue")
	}
	logger.Info().Str("component", "init").Str("queue", "watermark_retry").Msg("rabbitmq connecté")
	return ch
}

// ── Handlers ──────────────────────────────────────────────────────────────────

func handleUpload(w http.ResponseWriter, r *http.Request) {
	start := time.Now()

	// ── ① Lecture ────────────────────────────────────────
	file, header, err := r.FormFile("image")
	if err != nil {
		http.Error(w, "Image manquante", http.StatusBadRequest)
		return
	}
	defer file.Close()

	tRead := time.Now()
	data, err := io.ReadAll(file)
	if err != nil {
		http.Error(w, "Erreur lecture", http.StatusInternalServerError)
		return
	}
	readDur := time.Since(tRead)
	logger.Info().Str("step", "read").Str("filename", header.Filename).Str("size", formatBytes(len(data))).Dur("duration", readDur).Msg("lecture image")

	// ── ① bis Paramètres watermark + format de sortie ────
	wmText := r.FormValue("wm_text")
	if wmText == "" {
		wmText = "NWS © 2026"
	}
	wmPosition := r.FormValue("wm_position")
	if wmPosition == "" {
		wmPosition = "bottom-right"
	}
	// Négociation de format : WebP si le navigateur le supporte, JPEG sinon.
	// Même logique en fallback dans l'optimizer si wm_format est absent.
	wmFormat := bestFormat(r)
	logger.Info().Str("step", "format").Str("accept", r.Header.Get("Accept")).Str("chosen", wmFormat).Msg("négociation format")

	// ── ② Hash SHA256 ─────────────────────────────────────
	// La clé de cache inclut image + texte + position + format pour garantir l'unicité.
	// Un même upload avec Accept: image/webp et Accept: image/jpeg → deux entrées Redis distinctes.
	tHash := time.Now()
	hashInput := append(data, []byte(wmText+"|"+wmPosition+"|"+wmFormat)...)
	sum := sha256.Sum256(hashInput)
	cacheKey := hex.EncodeToString(sum[:])
	// La clé MinIO est basée sur l'image uniquement pour éviter de stocker N fois la même image.
	imgSum := sha256.Sum256(data)
	originalKey := "original/" + hex.EncodeToString(imgSum[:]) + ".jpg"
	hashDur := time.Since(tHash)
	logger.Info().Str("step", "hash").Str("key", cacheKey[:16]).Str("format", wmFormat).Dur("duration", hashDur).Msg("sha256")

	ctx := context.Background()

	// ── ③ Cache Redis ─────────────────────────────────────
	cached, redisDur, hit := getFromCache(ctx, cacheKey)
	if hit {
		logger.Info().Str("step", "cache").Bool("hit", true).Str("size", formatBytes(len(cached))).Dur("duration", redisDur).Msg("redis lookup")
		logger.Info().Str("step", "total").Dur("duration", time.Since(start)).Bool("cache_hit", true).Msg("requête terminée")
		w.Header().Set("X-Cache", "HIT")
		w.Header().Set("X-T-Read", fmtMs(readDur))
		w.Header().Set("X-T-Hash", fmtMs(hashDur))
		w.Header().Set("X-T-Redis", fmtMs(redisDur))
		w.Header().Set("Vary", "Accept")
		sendResponse(w, r, cached)
		return
	}
	logger.Info().Str("step", "cache").Bool("hit", false).Dur("duration", redisDur).Msg("redis lookup")

	// ── ④ Sauvegarde original dans MinIO ──────────────────
	minioDur := saveOriginal(ctx, originalKey, data)

	// ── ⑤ Forward vers l'optimizer ───────────────────────
	optimizerURL := os.Getenv("OPTIMIZER_URL")
	if optimizerURL == "" {
		optimizerURL = "http://localhost:3001"
	}

	tOptimizer := time.Now()
	result, err := sendToOptimizer(optimizerURL, header.Filename, data, wmText, wmPosition, wmFormat)
	if err != nil {
		logger.Error().Str("step", "optimizer").Err(err).Msg("optimizer KO")
		w.Header().Set("X-Cache", "MISS")
		w.Header().Set("X-T-Read", fmtMs(readDur))
		w.Header().Set("X-T-Hash", fmtMs(hashDur))
		w.Header().Set("X-T-Redis", fmtMs(redisDur))
		w.Header().Set("X-T-Minio", fmtMs(minioDur))
		replyWithRetryJob(w, ctx, cacheKey, originalKey, header.Filename, wmText, wmPosition, wmFormat, start)
		return
	}
	optimizerDur := time.Since(tOptimizer)
	logger.Info().Str("step", "optimizer").Str("format", wmFormat).Str("size", formatBytes(len(result))).Dur("duration", optimizerDur).Msg("image optimisée")

	// ── ⑥ Stockage Redis ──────────────────────────────────
	tStore := time.Now()
	redisClient.Set(ctx, cacheKey, result, 24*time.Hour)
	storeDur := time.Since(tStore)
	logger.Info().Str("step", "redis_set").Str("size", formatBytes(len(result))).Dur("duration", storeDur).Msg("résultat mis en cache")

	// ── ⑦ Réponse ─────────────────────────────────────────
	gzipped := strings.Contains(r.Header.Get("Accept-Encoding"), "gzip")
	logger.Info().Str("step", "response").Bool("gzip", gzipped).Str("format", wmFormat).Str("size", formatBytes(len(result))).Msg("envoi réponse")
	logger.Info().Str("step", "total").Dur("duration", time.Since(start)).Bool("cache_hit", false).Msg("requête terminée")

	w.Header().Set("X-Cache", "MISS")
	w.Header().Set("X-T-Read", fmtMs(readDur))
	w.Header().Set("X-T-Hash", fmtMs(hashDur))
	w.Header().Set("X-T-Redis", fmtMs(redisDur))
	w.Header().Set("X-T-Minio", fmtMs(minioDur))
	w.Header().Set("X-T-Optimizer", fmtMs(optimizerDur))
	w.Header().Set("X-T-Store", fmtMs(storeDur))
	w.Header().Set("Vary", "Accept")
	sendResponse(w, r, result)
}

func handleStatus(w http.ResponseWriter, r *http.Request) {
	hash := r.PathValue("hash")
	if hash == "" {
		http.Error(w, "Hash manquant", http.StatusBadRequest)
		return
	}

	exists, err := redisClient.Exists(context.Background(), hash).Result()
	if err != nil {
		http.Error(w, "Erreur Redis", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	if exists == 1 {
		json.NewEncoder(w).Encode(map[string]string{"status": "done", "url": "/image/" + hash})
	} else {
		json.NewEncoder(w).Encode(map[string]string{"status": "pending"})
	}
}

func handleGetImage(w http.ResponseWriter, r *http.Request) {
	hash := r.PathValue("hash")
	if hash == "" {
		http.Error(w, "Hash manquant", http.StatusBadRequest)
		return
	}

	data, err := redisClient.Get(context.Background(), hash).Bytes()
	if err != nil {
		http.Error(w, "Image introuvable", http.StatusNotFound)
		return
	}

	w.Header().Set("Vary", "Accept")
	sendResponse(w, r, data)
}

// ── Worker ────────────────────────────────────────────────────────────────────

func retryWorker() {
	wlog.Info().Str("queue", "watermark_retry").Msg("worker démarré")

	if err := amqpChan.Qos(1, 0, false); err != nil {
		wlog.Fatal().Err(err).Msg("impossible de configurer qos")
	}

	msgs, err := amqpChan.Consume("watermark_retry", "", false, false, false, false, nil)
	if err != nil {
		wlog.Fatal().Err(err).Str("queue", "watermark_retry").Msg("impossible de consommer la queue")
	}

	optimizerURL := os.Getenv("OPTIMIZER_URL")
	if optimizerURL == "" {
		optimizerURL = "http://localhost:3001"
	}

	for msg := range msgs {
		processRetryJob(msg, optimizerURL)
	}
}

func processRetryJob(msg amqp.Delivery, optimizerURL string) {
	start := time.Now()

	var job RetryJob
	if err := json.Unmarshal(msg.Body, &job); err != nil {
		wlog.Error().Err(err).Msg("message invalide - poison pill éliminé")
		msg.Ack(false)
		return
	}
	wlog.Info().Str("hash", job.Hash[:16]).Str("filename", job.Filename).Str("format", job.WmFormat).Msg("job reçu")

	// ── ① Récupérer l'original depuis MinIO ──────────────
	data, err := fetchFromMinio(job.OriginalKey)
	if err != nil {
		wlog.Error().Err(err).Str("step", "minio_get").Msg("minio KO - nack requeue dans 5s")
		msg.Nack(false, true)
		time.Sleep(5 * time.Second)
		return
	}

	// ── ② Retenter l'optimizer ───────────────────────────
	t := time.Now()
	result, err := sendToOptimizer(optimizerURL, job.Filename, data, job.WmText, job.WmPosition, job.WmFormat)
	if err != nil {
		wlog.Error().Err(err).Str("step", "optimizer").Msg("optimizer toujours KO - nack requeue dans 10s")
		msg.Nack(false, true)
		time.Sleep(10 * time.Second)
		return
	}
	wlog.Info().Str("step", "optimizer").Str("size", formatBytes(len(result))).Dur("duration", time.Since(t)).Msg("image optimisée")

	// ── ③ Stocker dans Redis ──────────────────────────────
	t = time.Now()
	redisClient.Set(context.Background(), job.Hash, result, 24*time.Hour)
	wlog.Info().Str("step", "redis_set").Str("size", formatBytes(len(result))).Dur("duration", time.Since(t)).Msg("résultat mis en cache")

	msg.Ack(false)
	wlog.Info().Str("hash", job.Hash[:16]).Dur("duration", time.Since(start)).Msg("ack envoyé")
}

// ── Helpers ───────────────────────────────────────────────────────────────────

// bestFormat lit le header Accept et retourne "webp" ou "jpeg".
// WebP offre ~30% de réduction par rapport à JPEG à qualité visuelle équivalente.
func bestFormat(r *http.Request) string {
	if strings.Contains(r.Header.Get("Accept"), "image/webp") {
		return "webp"
	}
	return "jpeg"
}

// detectContentType identifie le format à partir des magic bytes.
// Utilisé pour fixer le Content-Type correct sur les réponses cache HIT
// sans avoir besoin de stocker le type séparément dans Redis.
//
// Magic bytes : WebP = "RIFF????WEBP" | JPEG = 0xFF 0xD8
func detectContentType(data []byte) string {
	if len(data) >= 12 &&
		data[0] == 'R' && data[1] == 'I' && data[2] == 'F' && data[3] == 'F' &&
		data[8] == 'W' && data[9] == 'E' && data[10] == 'B' && data[11] == 'P' {
		return "image/webp"
	}
	return "image/jpeg"
}

// getFromCache vérifie le cache Redis et retourne les données + durée du lookup.
func getFromCache(ctx context.Context, key string) ([]byte, time.Duration, bool) {
	t := time.Now()
	data, err := redisClient.Get(ctx, key).Bytes()
	return data, time.Since(t), err == nil
}

// saveOriginal sauvegarde l'image originale dans MinIO et retourne la durée.
func saveOriginal(ctx context.Context, key string, data []byte) time.Duration {
	t := time.Now()
	_, err := minioClient.PutObject(ctx, minioBucket, key, bytes.NewReader(data), int64(len(data)), minio.PutObjectOptions{
		ContentType: "image/jpeg",
	})
	// Même si la sauvegarde échoue, on continue quand même le process (on ne veut pas pénaliser l'utilisateur final pour un problème de stockage), mais on logue l'erreur et la durée.
	//dur c'est la durée de l'appel à PutObject, pas la durée totale du process, pour isoler le temps de stockage dans les logs.
	//(t) c'est le temps de début de l'appel à PutObject, pas le temps de début du process, pour isoler le temps de stockage dans les logs.
	dur := time.Since(t)
	if err != nil {
		logger.Error().Err(err).Str("step", "minio_put").Str("key", key).Msg("sauvegarde original échouée")
	} else {
		logger.Info().Str("step", "minio_put").Str("size", formatBytes(len(data))).Dur("duration", dur).Msg("original sauvegardé")
	}
	return dur
}

// fetchFromMinio récupère l'image originale depuis MinIO.
func fetchFromMinio(key string) ([]byte, error) {
	t := time.Now()
	obj, err := minioClient.GetObject(context.Background(), minioBucket, key, minio.GetObjectOptions{})
	if err != nil {
		return nil, err
	}
	defer obj.Close()
	data, err := io.ReadAll(obj)
	if err != nil || len(data) == 0 {
		return nil, fmt.Errorf("lecture échouée ou fichier vide")
	}
	wlog.Info().Str("step", "minio_get").Str("size", formatBytes(len(data))).Dur("duration", time.Since(t)).Msg("original récupéré")
	return data, nil
}

// publishRetryJob publie un job dans RabbitMQ quand l'optimizer est KO.
func publishRetryJob(ctx context.Context, hash, originalKey, filename, wmText, wmPosition, wmFormat string) error {
	job := RetryJob{Hash: hash, OriginalKey: originalKey, Filename: filename, WmText: wmText, WmPosition: wmPosition, WmFormat: wmFormat}
	body, _ := json.Marshal(job)

	t := time.Now()
	err := amqpChan.PublishWithContext(ctx, "", "watermark_retry", false, false,
		amqp.Publishing{
			DeliveryMode: amqp.Persistent,
			ContentType:  "application/json",
			Body:         body,
		},
	)
	if err != nil {
		logger.Error().Err(err).Str("step", "rabbitmq_publish").Msg("publish échoué")
		return err
	}
	logger.Info().Str("step", "rabbitmq_publish").Str("queue", "watermark_retry").Dur("duration", time.Since(t)).Msg("job publié")
	return nil
}

// replyWithRetryJob publie le job RabbitMQ et répond 202 au client.
func replyWithRetryJob(w http.ResponseWriter, ctx context.Context, cacheKey, originalKey, filename, wmText, wmPosition, wmFormat string, start time.Time) {
	tRabbit := time.Now()
	if err := publishRetryJob(ctx, cacheKey, originalKey, filename, wmText, wmPosition, wmFormat); err != nil {
		http.Error(w, "Microservice indisponible", http.StatusBadGateway)
		return
	}
	w.Header().Set("X-T-Rabbit", fmtMs(time.Since(tRabbit)))
	logger.Info().Str("step", "response").Int("status", 202).Str("job_id", cacheKey[:16]).Msg("202 accepted")
	logger.Info().Str("step", "total").Dur("duration", time.Since(start)).Msg("requête terminée")

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	json.NewEncoder(w).Encode(map[string]string{"jobId": cacheKey})
}

// sendToOptimizer envoie l'image à l'optimizer via HTTP multipart et retourne le résultat.
func sendToOptimizer(optimizerURL, filename string, data []byte, wmText, wmPosition, wmFormat string) ([]byte, error) {
	pr, pw := io.Pipe()
	mw := multipart.NewWriter(pw)

	go func() {
		part, err := mw.CreateFormFile("image", filename)
		if err != nil {
			pw.CloseWithError(err)
			return
		}
		io.Copy(part, bytes.NewReader(data))
		mw.WriteField("wm_text", wmText)
		mw.WriteField("wm_position", wmPosition)
		mw.WriteField("wm_format", wmFormat)
		mw.Close()
		pw.Close()
	}()

	resp, err := httpClient.Post(optimizerURL+"/optimize", mw.FormDataContentType(), pr)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	return io.ReadAll(resp.Body)
}

// sendResponse envoie les données au client avec le bon Content-Type (détecté par magic bytes)
// et compression gzip si le navigateur le supporte.
func sendResponse(w http.ResponseWriter, r *http.Request, data []byte) {
	// detectContentType évite de stocker le type séparément dans Redis :
	// on lit les 12 premiers octets de la réponse pour identifier JPEG vs WebP.
	ct := detectContentType(data)
	w.Header().Set("Content-Type", ct)

	if strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
		w.Header().Set("Content-Encoding", "gzip")
		gz, err := gzip.NewWriterLevel(w, gzip.BestSpeed)
		if err != nil {
			http.Error(w, "Erreur compression", http.StatusInternalServerError)
			return
		}
		defer gz.Close()
		gz.Write(data)
	} else {
		w.Write(data)
	}
}

func corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
		w.Header().Set("Access-Control-Expose-Headers", "X-Cache, X-T-Read, X-T-Hash, X-T-Redis, X-T-Minio, X-T-Optimizer, X-T-Store, X-T-Rabbit")

		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}

		next.ServeHTTP(w, r)
	})
}

// fmtMs convertit une duration en millisecondes formatées (ex: "1156.844")
func fmtMs(d time.Duration) string {
	return fmt.Sprintf("%.3f", float64(d.Microseconds())/1000)
}

func formatBytes(b int) string {
	if b < 1024 {
		return fmt.Sprintf("%d B", b)
	} else if b < 1024*1024 {
		return fmt.Sprintf("%.1f KB", float64(b)/1024)
	}
	return fmt.Sprintf("%.1f MB", float64(b)/1024/1024)
}
