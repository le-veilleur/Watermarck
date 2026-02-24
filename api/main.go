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
	"log"
	"mime/multipart"
	"net/http"
	"os"
	"strings"
	"time"

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

// RetryJob représente un job publié dans RabbitMQ quand l'optimizer est KO.
type RetryJob struct {
	Hash        string `json:"hash"`
	OriginalKey string `json:"original_key"`
	Filename    string `json:"filename"`
	WmText      string `json:"wm_text"`
	WmPosition  string `json:"wm_position"`
}

// ── Main ─────────────────────────────────────────────────────────────────────
func main() {

	redisClient = initRedis()
	minioClient = initMinio()
	amqpChan = initRabbitMQ()

	go retryWorker()

	log.Println("[API] Démarrage sur :3000")
	log.Println("[API] ─────────────────────────────────────────")

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
		log.Fatalf("[API] Redis URL invalide : %v", err)
	}
	client := redis.NewClient(opt)
	if err := client.Ping(context.Background()).Err(); err != nil {
		log.Fatalf("[API] Impossible de se connecter à Redis : %v", err)
	}
	log.Println("[API] ✓ Redis connecté")
	return client
}

// 
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
		log.Fatalf("[API] MinIO client invalide : %v", err)
	}

	ctx := context.Background()
	exists, err := client.BucketExists(ctx, minioBucket)
	if err != nil {
		log.Fatalf("[API] MinIO inaccessible : %v", err)
	}
	if !exists {
		if err := client.MakeBucket(ctx, minioBucket, minio.MakeBucketOptions{}); err != nil {
			log.Fatalf("[API] Impossible de créer le bucket MinIO : %v", err)
		}
		log.Printf("[API] ✓ MinIO bucket '%s' créé", minioBucket)
	} else {
		log.Printf("[API] ✓ MinIO connecté | bucket '%s' existant", minioBucket)
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
		log.Fatalf("[API] Impossible de se connecter à RabbitMQ : %v", err)
	}
	ch, err := conn.Channel()
	if err != nil {
		log.Fatalf("[API] Impossible d'ouvrir un channel RabbitMQ : %v", err)
	}
	_, err = ch.QueueDeclare("watermark_retry", true, false, false, false, nil)
	if err != nil {
		log.Fatalf("[API] Impossible de déclarer la queue RabbitMQ : %v", err)
	}
	log.Println("[API] ✓ RabbitMQ connecté | queue 'watermark_retry' déclarée")
	return ch
}

// ── Handlers ──────────────────────────────────────────────────────────────────

func handleUpload(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	log.Println("[API] ════════════════════════════════════════")
	log.Println("[API] → Nouvelle requête reçue")

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
	log.Printf("[API] ① Lecture    : %s | %s | %v", header.Filename, formatBytes(len(data)), readDur)

	// ── ① bis Paramètres watermark ───────────────────────
	wmText := r.FormValue("wm_text")
	if wmText == "" {
		wmText = "NWS © 2026"
	}
	wmPosition := r.FormValue("wm_position")
	if wmPosition == "" {
		wmPosition = "bottom-right"
	}

	// ── ② Hash SHA256 ─────────────────────────────────────
	// La clé de cache inclut image + texte + position pour garantir l'unicité.
	tHash := time.Now()
	hashInput := append(data, []byte(wmText+"|"+wmPosition)...)
	sum := sha256.Sum256(hashInput)
	cacheKey := hex.EncodeToString(sum[:])
	// La clé MinIO est basée sur l'image uniquement pour éviter de stocker N fois la même image.
	imgSum := sha256.Sum256(data)
	originalKey := "original/" + hex.EncodeToString(imgSum[:]) + ".jpg"
	hashDur := time.Since(tHash)
	log.Printf("[API] ② SHA256     : %s... | calculé en %v", cacheKey[:16], hashDur)

	ctx := context.Background()

	// ── ③ Cache Redis ─────────────────────────────────────
	cached, redisDur, hit := getFromCache(ctx, cacheKey)
	if hit {
		log.Printf("[API] ③ Redis      : ✅ CACHE HIT  | %s récupérés en %v", formatBytes(len(cached)), redisDur)
		log.Printf("[API] ⚡ Total      : %v (sans passer par l'optimizer !)", time.Since(start))
		log.Println("[API] ════════════════════════════════════════")
		w.Header().Set("X-Cache", "HIT")
		w.Header().Set("X-T-Read", fmtMs(readDur))
		w.Header().Set("X-T-Hash", fmtMs(hashDur))
		w.Header().Set("X-T-Redis", fmtMs(redisDur))
		sendResponse(w, r, cached)
		return
	}
	log.Printf("[API] ③ Redis      : ❌ CACHE MISS | lookup en %v", redisDur)

	// ── ④ Sauvegarde original dans MinIO ──────────────────
	minioDur := saveOriginal(ctx, originalKey, data)

	// ── ⑤ Forward vers l'optimizer ───────────────────────
	optimizerURL := os.Getenv("OPTIMIZER_URL")
	if optimizerURL == "" {
		optimizerURL = "http://localhost:3001"
	}

	tOptimizer := time.Now()
	result, err := sendToOptimizer(optimizerURL, header.Filename, data, wmText, wmPosition)
	if err != nil {
		log.Printf("[API] ⑤ Optimizer  : ❌ KO : %v", err)
		w.Header().Set("X-Cache", "MISS")
		w.Header().Set("X-T-Read", fmtMs(readDur))
		w.Header().Set("X-T-Hash", fmtMs(hashDur))
		w.Header().Set("X-T-Redis", fmtMs(redisDur))
		w.Header().Set("X-T-Minio", fmtMs(minioDur))
		replyWithRetryJob(w, ctx, cacheKey, originalKey, header.Filename, wmText, wmPosition, start)
		return
	}
	optimizerDur := time.Since(tOptimizer)
	log.Printf("[API] ⑤ Optimizer  : ✓ %s reçus en %v", formatBytes(len(result)), optimizerDur)

	// ── ⑥ Stockage Redis ──────────────────────────────────
	tStore := time.Now()
	redisClient.Set(ctx, cacheKey, result, 24*time.Hour)
	storeDur := time.Since(tStore)
	log.Printf("[API] ⑥ Redis.Set  : ✓ %s stockés | TTL 24h | en %v", formatBytes(len(result)), storeDur)

	// ── ⑦ Réponse ─────────────────────────────────────────
	log.Printf("[API] ⑦ Réponse    : gzip=%v | taille=%s", strings.Contains(r.Header.Get("Accept-Encoding"), "gzip"), formatBytes(len(result)))
	log.Printf("[API] ⏱ Total      : %v", time.Since(start))
	log.Println("[API] ════════════════════════════════════════")

	w.Header().Set("X-Cache", "MISS")
	w.Header().Set("X-T-Read", fmtMs(readDur))
	w.Header().Set("X-T-Hash", fmtMs(hashDur))
	w.Header().Set("X-T-Redis", fmtMs(redisDur))
	w.Header().Set("X-T-Minio", fmtMs(minioDur))
	w.Header().Set("X-T-Optimizer", fmtMs(optimizerDur))
	w.Header().Set("X-T-Store", fmtMs(storeDur))
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

	sendResponse(w, r, data)
}

// ── Worker ────────────────────────────────────────────────────────────────────

func retryWorker() {
	log.Println("[Worker] ✓ Démarrage du worker retry RabbitMQ | queue 'watermark_retry'")

	if err := amqpChan.Qos(1, 0, false); err != nil {
		log.Fatalf("[Worker] Impossible de configurer Qos : %v", err)
	}

	msgs, err := amqpChan.Consume("watermark_retry", "", false, false, false, false, nil)
	if err != nil {
		log.Fatalf("[Worker] Impossible de consommer la queue : %v", err)
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
	log.Println("[Worker] ════════════════════════════════════════")

	var job RetryJob
	if err := json.Unmarshal(msg.Body, &job); err != nil {
		log.Printf("[Worker] ❌ Message invalide : %v → ACK (élimination poison pill)", err)
		log.Println("[Worker] ════════════════════════════════════════")
		msg.Ack(false)
		return
	}
	log.Printf("[Worker] → Job reçu | hash=%s... | filename=%s", job.Hash[:16], job.Filename)

	// ── ① Récupérer l'original depuis MinIO ──────────────
	data, err := fetchFromMinio(job.OriginalKey)
	if err != nil {
		log.Printf("[Worker] ① MinIO.Get  : ❌ %v → NACK (requeue dans 5s)", err)
		log.Println("[Worker] ════════════════════════════════════════")
		msg.Nack(false, true)
		time.Sleep(5 * time.Second)
		return
	}

	// ── ② Retenter l'optimizer ───────────────────────────
	t := time.Now()
	result, err := sendToOptimizer(optimizerURL, job.Filename, data, job.WmText, job.WmPosition)
	if err != nil {
		log.Printf("[Worker] ② Optimizer  : ❌ Toujours KO : %v → NACK (requeue dans 10s)", err)
		log.Println("[Worker] ════════════════════════════════════════")
		msg.Nack(false, true)
		time.Sleep(10 * time.Second)
		return
	}
	log.Printf("[Worker] ② Optimizer  : ✓ %s reçus | en %v", formatBytes(len(result)), time.Since(t))

	// ── ③ Stocker dans Redis ──────────────────────────────
	t = time.Now()
	redisClient.Set(context.Background(), job.Hash, result, 24*time.Hour)
	log.Printf("[Worker] ③ Redis.Set  : ✓ %s stockés | TTL 24h | en %v", formatBytes(len(result)), time.Since(t))

	msg.Ack(false)
	log.Printf("[Worker] ✅ ACK envoyé | hash=%s... | ⏱ total %v", job.Hash[:16], time.Since(start))
	log.Println("[Worker] ════════════════════════════════════════")
}

// ── Helpers ───────────────────────────────────────────────────────────────────

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
		log.Printf("[API] ④ MinIO.Put  : ⚠ Sauvegarde original échouée : %v", err)
	} else {
		log.Printf("[API] ④ MinIO.Put  : ✓ Original sauvegardé | %s | en %v", formatBytes(len(data)), dur)
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
	log.Printf("[Worker] ① MinIO.Get  : ✓ %s récupérés | en %v", formatBytes(len(data)), time.Since(t))
	return data, nil
}

// publishRetryJob publie un job dans RabbitMQ quand l'optimizer est KO.
func publishRetryJob(ctx context.Context, hash, originalKey, filename, wmText, wmPosition string) error {
	job := RetryJob{Hash: hash, OriginalKey: originalKey, Filename: filename, WmText: wmText, WmPosition: wmPosition}
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
		log.Printf("[API] ⑥ RabbitMQ   : ❌ Publish échoué : %v", err)
		return err
	}
	log.Printf("[API] ⑥ RabbitMQ   : ✅ Job publié | queue=watermark_retry | persistent | en %v", time.Since(t))
	return nil
}

// replyWithRetryJob publie le job RabbitMQ et répond 202 au client.
func replyWithRetryJob(w http.ResponseWriter, ctx context.Context, cacheKey, originalKey, filename, wmText, wmPosition string, start time.Time) {
	tRabbit := time.Now()
	if err := publishRetryJob(ctx, cacheKey, originalKey, filename, wmText, wmPosition); err != nil {
		http.Error(w, "Microservice indisponible", http.StatusBadGateway)
		return
	}
	w.Header().Set("X-T-Rabbit", fmtMs(time.Since(tRabbit)))
	log.Printf("[API] ⑦ Réponse    : 202 Accepted | jobId=%s... | poll → /status/%s...", cacheKey[:16], cacheKey[:16])
	log.Printf("[API] ⏱ Total      : %v", time.Since(start))
	log.Println("[API] ════════════════════════════════════════")

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	json.NewEncoder(w).Encode(map[string]string{"jobId": cacheKey})
}

// sendToOptimizer envoie l'image à l'optimizer via HTTP multipart et retourne le résultat.
func sendToOptimizer(optimizerURL, filename string, data []byte, wmText, wmPosition string) ([]byte, error) {
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

func sendResponse(w http.ResponseWriter, r *http.Request, data []byte) {
	w.Header().Set("Content-Type", "image/jpeg")

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
