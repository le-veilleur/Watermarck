package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log"
	"mime/multipart"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
	"github.com/redis/go-redis/v9"
)

// Ce microservice API reçoit une image, vérifie si une version optimisée existe déjà dans Redis (cache), sinon il la forward à l'optimizer, stocke le résultat dans Redis et MinIO, puis renvoie l'image optimisée au client. Il utilise un http.Client partagé avec timeout et keep-alive pour les requêtes vers l'optimizer, et logue chaque étape du processus.
const minioBucket = "watermarks"

var httpClient = &http.Client{
	Timeout: 30 * time.Second,
}

var redisClient *redis.Client
// MinIO client global pour éviter de le recréer à chaque requête
var minioClient *minio.Client

func main() {
	redisURL := os.Getenv("REDIS_URL")
	if redisURL == "" {
		redisURL = "redis://localhost:6379"
	}

	opt, err := redis.ParseURL(redisURL)
	if err != nil {
		log.Fatalf("[API] Redis URL invalide : %v", err)
	}

	redisClient = redis.NewClient(opt)

	if err := redisClient.Ping(context.Background()).Err(); err != nil {
		log.Fatalf("[API] Impossible de se connecter à Redis : %v", err)
	}
	log.Println("[API] ✓ Redis connecté")
	// ── Initialisation MinIO ───────────────────────────────
	// Les paramètres de connexion à MinIO sont récupérés depuis les variables d'environnement, avec des valeurs par défaut pour faciliter le développement local.
	minioEndpoint := os.Getenv("MINIO_ENDPOINT")
	// Si MINIO_ENDPOINT n'est pas défini, on utilise localhost:9000, qui est le port par défaut de MinIO en local.
	if minioEndpoint == "" {
		// En développement local, on suppose que MinIO tourne sur localhost:9000
		minioEndpoint = "localhost:9000"
	}
	// Les identifiants d'accès à MinIO sont également récupérés depuis les variables d'environnement, avec des valeurs par défaut "minioadmin" pour faciliter le développement local.
	minioUser := os.Getenv("MINIO_ROOT_USER")
	// Si MINIO_ROOT_USER n'est pas défini, on utilise "minioadmin", qui est le nom d'utilisateur par défaut de MinIO en local.
	if minioUser == "" {
		// En développement local, on suppose que les identifiants par défaut de MinIO sont utilisés
		minioUser = "minioadmin"
	}
	// Le mot de passe pour MinIO est également récupéré depuis les variables d'environnement, avec une valeur par défaut "minioadmin" pour faciliter le développement local.
	minioPassword := os.Getenv("MINIO_ROOT_PASSWORD")
	// Si MINIO_ROOT_PASSWORD n'est pas défini, on utilise "minioadmin", qui est le mot de passe par défaut de MinIO en local.
	if minioPassword == "" {
		// En développement local, on suppose que les identifiants par défaut de MinIO sont utilisés
		minioPassword = "minioadmin"
	}

	// Création du client MinIO avec les paramètres de connexion
	minioClient, err = minio.New(minioEndpoint, &minio.Options{
		// On utilise des identifiants statiques pour se connecter à MinIO, avec les valeurs récupérées depuis les variables d'environnement ou les valeurs par défaut.
		Creds:  credentials.NewStaticV4(minioUser, minioPassword, ""),
		// En développement local, MinIO n'utilise pas TLS, donc Secure est à false. En production, il faudrait configurer TLS et mettre Secure à true.
		Secure: false,
	})
	// Si la création du client MinIO échoue, on logue une erreur critique et on arrête le programme, car l'accès à MinIO est essentiel pour le fonctionnement de l'API.
	if err != nil {
		// En cas d'erreur lors de la création du client MinIO, on logue une erreur critique et on arrête le programme, car l'accès à MinIO est essentiel pour le fonctionnement de l'API.
		log.Fatalf("[API] MinIO client invalide : %v", err)
	}

	// Vérification de l'existence du bucket MinIO et création si nécessaire
	ctx := context.Background()
	// On vérifie si le bucket spécifié existe déjà dans MinIO. Si une erreur survient lors de cette vérification, on logue une erreur critique et on arrête le programme, car cela signifie que MinIO est inaccessible.
	exists, err := minioClient.BucketExists(ctx, minioBucket)
	// Si une erreur survient lors de la vérification de l'existence du bucket, cela signifie que MinIO est inaccessible. On logue une erreur critique et on arrête le programme, car l'accès à MinIO est essentiel pour le fonctionnement de l'API.
	if err != nil {
		// En cas d'erreur lors de la vérification de l'existence du bucket, on logue une erreur critique et on arrête le programme, car cela signifie que MinIO est inaccessible.
		log.Fatalf("[API] MinIO inaccessible : %v", err)
	}
	// Si le bucket n'existe pas, on tente de le créer. Si la création échoue, on logue une erreur critique et on arrête le programme, car l'accès à MinIO est essentiel pour le fonctionnement de l'API.
	if !exists {
		// Si le bucket n'existe pas, on tente de le créer. Si la création échoue, on logue une erreur critique et on arrête le programme, car l'accès à MinIO est essentiel pour le fonctionnement de l'API.
		if err := minioClient.MakeBucket(ctx, minioBucket, minio.MakeBucketOptions{}); err != nil {
			// En cas d'erreur lors de la création du bucket, on logue une erreur critique et on arrête le programme, car l'accès à MinIO est essentiel pour le fonctionnement de l'API.
			log.Fatalf("[API] Impossible de créer le bucket MinIO : %v", err)
		}
		// Si le bucket a été créé avec succès, on logue une confirmation.
		log.Printf("[API] ✓ MinIO bucket '%s' créé", minioBucket)

	} else {
		// Si le bucket existe déjà, on logue une confirmation.
		log.Printf("[API] ✓ MinIO connecté | bucket '%s' existant", minioBucket)
	}

	log.Println("[API] ✓ HTTP client partagé initialisé (timeout 30s, keep-alive activé)")
	log.Println("[API] Démarrage sur :3000")
	log.Println("[API] ─────────────────────────────────────────")

	mux := http.NewServeMux()
	mux.HandleFunc("POST /upload", handleUpload)
	mux.HandleFunc("GET /image/{hash}", handleGetImage)

	http.ListenAndServe(":3000", corsMiddleware(mux))
}

func handleUpload(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	log.Println("[API] ════════════════════════════════════════")
	log.Println("[API] → Nouvelle requête reçue")

	file, header, err := r.FormFile("image")
	if err != nil {
		http.Error(w, "Image manquante", http.StatusBadRequest)
		return
	}
	defer file.Close()

	// ── Étape 1 : Lecture ────────────────────────────────
	t := time.Now()
	data, err := io.ReadAll(file)
	if err != nil {
		http.Error(w, "Erreur lecture", http.StatusInternalServerError)
		return
	}
	log.Printf("[API] ① Lecture    : %s | %s | %v",
		header.Filename,
		formatBytes(len(data)),
		time.Since(t),
	)

	// ── Étape 2 : Hash SHA256 ────────────────────────────
	t = time.Now()
	hash := sha256.Sum256(data)
	cacheKey := hex.EncodeToString(hash[:])
	log.Printf("[API] ② SHA256     : %s... | calculé en %v", cacheKey[:16], time.Since(t))

	ctx := context.Background()

	// ── Étape 3 : Vérification cache Redis ───────────────
	t = time.Now()
	cached, err := redisClient.Get(ctx, cacheKey).Bytes()
	redisDuration := time.Since(t)

	if err == nil {
		log.Printf("[API] ③ Redis      : ✅ CACHE HIT  | %s récupérés en %v", formatBytes(len(cached)), redisDuration)
		log.Printf("[API] ⚡ Total      : %v (sans passer par l'optimizer !)", time.Since(start))
		log.Println("[API] ════════════════════════════════════════")
		sendResponse(w, r, cached)
		return
	}

	log.Printf("[API] ③ Redis      : ❌ CACHE MISS | lookup en %v", redisDuration)

	// ── Étape 4 : Sauvegarde original dans MinIO ─────────
	t = time.Now()
	originalKey := "original/" + cacheKey + ".jpg"
	_, err = minioClient.PutObject(ctx, minioBucket, originalKey, bytes.NewReader(data), int64(len(data)), minio.PutObjectOptions{
		ContentType: "image/jpeg",
	})
	if err != nil {
		log.Printf("[API] ④ MinIO.Put  : ⚠ Sauvegarde original échouée : %v", err)
	} else {
		log.Printf("[API] ④ MinIO.Put  : ✓ Original sauvegardé | %s | en %v", formatBytes(len(data)), time.Since(t))
	}

	// ── Étape 5 : Forward vers l'optimizer ───────────────
	optimizerURL := os.Getenv("OPTIMIZER_URL")
	if optimizerURL == "" {
		optimizerURL = "http://localhost:3001"
	}

	result, err := sendToOptimizer(optimizerURL, header.Filename, data)
	if err != nil {
		// ── Étape 5b : Optimizer KO → reprise depuis MinIO ───
		log.Printf("[API] ⑤ Optimizer  : ❌ Erreur : %v → tentative reprise depuis MinIO", err)
		obj, merr := minioClient.GetObject(ctx, minioBucket, originalKey, minio.GetObjectOptions{})
		if merr != nil {
			log.Printf("[API] ⑤ MinIO.Get  : ❌ Reprise impossible : %v", merr)
			http.Error(w, "Microservice indisponible", http.StatusBadGateway)
			return
		}
		recovered, merr := io.ReadAll(obj)
		obj.Close()
		if merr != nil || len(recovered) == 0 {
			log.Printf("[API] ⑤ MinIO.Get  : ❌ Lecture reprise échouée")
			http.Error(w, "Microservice indisponible", http.StatusBadGateway)
			return
		}
		log.Printf("[API] ⑤ MinIO.Get  : ✅ Original récupéré depuis MinIO → 2ème tentative optimizer")
		result, err = sendToOptimizer(optimizerURL, header.Filename, recovered)
		if err != nil {
			log.Printf("[API] ⑤ Optimizer  : ❌ 2ème tentative échouée : %v", err)
			http.Error(w, "Microservice indisponible", http.StatusBadGateway)
			return
		}
	}
	log.Printf("[API] ⑤ Optimizer  : ✓ %s reçus en %v", formatBytes(len(result)), time.Since(t))

	// ── Étape 6 : Stockage Redis ──────────────────────────
	t = time.Now()
	redisClient.Set(ctx, cacheKey, result, 24*time.Hour)
	log.Printf("[API] ⑥ Redis.Set  : ✓ %s stockés | TTL 24h | en %v", formatBytes(len(result)), time.Since(t))

	// ── Étape 7 : Réponse ────────────────────────────────
	gzipAccepted := strings.Contains(r.Header.Get("Accept-Encoding"), "gzip")
	log.Printf("[API] ⑦ Réponse    : gzip=%v | taille originale=%s", gzipAccepted, formatBytes(len(result)))
	log.Printf("[API] ⏱ Total      : %v", time.Since(start))
	log.Println("[API] ════════════════════════════════════════")

	sendResponse(w, r, result)
}

// sendToOptimizer envoie l'image à l'optimizer via une requête HTTP POST multipart/form-data et retourne la réponse optimisée. Il utilise un pipe pour éviter de charger tout le contenu en mémoire lors de l'envoi.
func sendToOptimizer(optimizerURL, filename string, data []byte) ([]byte, error) {
	// On utilise un pipe pour éviter de charger tout le contenu en mémoire lors de l'envoi à l'optimizer. Le multipart.Writer écrit directement dans le pipe, qui est lu par httpClient.Post.
	pr, pw := io.Pipe()
	// Le multipart.Writer est utilisé pour construire la requête multipart/form-data. Il écrit directement dans le pipe, ce qui permet d'envoyer les données à l'optimizer sans les stocker entièrement en mémoire.
	mw := multipart.NewWriter(pw)

	// On lance une goroutine pour écrire les données dans le multipart.Writer. Cela permet de construire la requête pendant que httpClient.Post lit le pipe, évitant ainsi de charger tout le contenu en mémoire.
	go func() {
		// On crée une partie pour le champ "image" avec le nom de fichier. Si une erreur survient, on ferme le pipe avec l'erreur pour signaler à httpClient.Post que l'envoi a échoué.
		part, err := mw.CreateFormFile("image", filename)
		// Si une erreur survient lors de la création de la partie multipart, on ferme le pipe avec l'erreur pour signaler à httpClient.Post que l'envoi a échoué.
		if err != nil {
			// En cas d'erreur lors de la création de la partie multipart, on ferme le pipe avec l'erreur pour signaler à httpClient.Post que l'envoi a échoué.
			pw.CloseWithError(err)
			// En cas d'erreur lors de la création de la partie multipart, on ferme le multipart.Writer pour libérer les ressources, même si httpClient.Post ne pourra pas lire les données.
			return
		}
		// On copie les données de l'image dans la partie multipart. Si une erreur survient, on ferme le pipe avec l'erreur pour signaler à httpClient.Post que l'envoi a échoué.
		io.Copy(part, bytes.NewReader(data))
		// Après avoir écrit les données, on ferme le multipart.Writer pour signaler que la construction de la requête est terminée. Cela permet à httpClient.Post de savoir que toutes les données ont été envoyées.
		mw.Close()
		// En fermant le multipart.Writer, on signale à httpClient.Post que la construction de la requête est terminée. Cela permet à httpClient.Post de savoir que toutes les données ont été envoyées.
		pw.Close()
	}()

	// On envoie la requête POST à l'optimizer en lisant directement depuis le pipe. Si une erreur survient, on la retourne pour que le handler puisse gérer le cas de l'optimizer indisponible.
	resp, err := httpClient.Post(optimizerURL+"/optimize", mw.FormDataContentType(), pr)
	// Si une erreur survient lors de l'envoi de la requête à l'optimizer, on retourne l'erreur pour que le handler puisse gérer le cas de l'optimizer indisponible.
	if err != nil {
		// En cas d'erreur lors de l'envoi de la requête à l'optimizer, on retourne l'erreur pour que le handler puisse gérer le cas de l'optimizer indisponible.
		return nil, err
	}
	// Si l'optimizer répond avec un code d'erreur, on retourne une erreur pour que le handler puisse gérer le cas de l'optimizer indisponible.
	defer resp.Body.Close()

	// Si l'optimizer répond avec un code d'erreur, on retourne une erreur pour que le handler puisse gérer le cas de l'optimizer indisponible.
	result, err := io.ReadAll(resp.Body)
	// Si une erreur survient lors de la lecture de la réponse de l'optimizer, on retourne l'erreur pour que le handler puisse gérer le cas de l'optimizer indisponible.
	if err != nil {
		// En cas d'erreur lors de la lecture de la réponse de l'optimizer, on retourne l'erreur pour que le handler puisse gérer le cas de l'optimizer indisponible.
		return nil, err
	}
	// Si l'optimizer répond avec un code d'erreur, on retourne une erreur pour que le handler puisse gérer le cas de l'optimizer indisponible.
	return result, nil
}

func handleGetImage(w http.ResponseWriter, r *http.Request) {
	hash := r.PathValue("hash")
	if hash == "" {
		http.Error(w, "Hash manquant", http.StatusBadRequest)
		return
	}

	objectName := hash + ".jpg"
	obj, err := minioClient.GetObject(r.Context(), minioBucket, objectName, minio.GetObjectOptions{})
	if err != nil {
		http.Error(w, "Objet introuvable", http.StatusNotFound)
		return
	}
	defer obj.Close()

	info, err := obj.Stat()
	if err != nil {
		http.Error(w, "Objet introuvable dans MinIO", http.StatusNotFound)
		return
	}

	w.Header().Set("Content-Type", "image/jpeg")
	w.Header().Set("Content-Length", fmt.Sprintf("%d", info.Size))
	io.Copy(w, obj)
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
		w.Header().Set("Access-Control-Allow-Methods", "POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")

		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}

		next.ServeHTTP(w, r)
	})
}

// formatBytes convertit des bytes en format lisible (KB, MB)
func formatBytes(b int) string {
	if b < 1024 {
		return fmt.Sprintf("%d B", b)
	} else if b < 1024*1024 {
		return fmt.Sprintf("%.1f KB", float64(b)/1024)
	}
	return fmt.Sprintf("%.1f MB", float64(b)/1024/1024)
}
