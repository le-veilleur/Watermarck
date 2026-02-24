# Roadmap : Serveur Haute Performance
## Tout ce qu'on peut apprendre et implémenter sur NWS Watermark

---

## 📋 Table des matières

1. [État actuel du projet](#actuel)
2. [Observabilité — voir ce qui se passe](#observabilite)
3. [Résilience — ne pas tomber sous charge](#resilience)
4. [Performance Go — le runtime en profondeur](#go-perf)
5. [Cache avancé — aller plus loin que Redis](#cache)
6. [HTTP avancé — protocoles modernes](#http)
7. [Optimisation image — formats et parallélisme](#image)
8. [Architecture distribuée — scaler horizontalement](#distribue)
9. [Linux & OS — le niveau zéro](#linux)
10. [Load testing — mesurer avant d'optimiser](#load-testing)
11. [Chaos Engineering — tester les pannes](#chaos)
12. [Sécurité — ce qui manque en prod](#securite)
13. [Ordre d'apprentissage suggéré](#ordre)

---

<a name="actuel"></a>
## 1. État actuel du projet

Ce qu'on a déjà implémenté et ce que ça couvre :

```
NWS Watermark — ce qui est fait
│
├── API (Go)
│   ├── ✅ Cache Redis (SHA256 → image watermarkée, TTL 24h)
│   ├── ✅ Stockage MinIO (original dédupliqué par hash image)
│   ├── ✅ Fallback RabbitMQ (si optimizer KO → job persistent)
│   ├── ✅ gzip (Content-Encoding négocié via Accept-Encoding)
│   ├── ✅ io.Pipe (streaming multipart sans buffer intermédiaire)
│   └── ✅ Cache key = SHA256(image + wm_text|wm_position)
│
├── Optimizer (Go)
│   ├── ✅ Worker pool (semaphore = 1 slot/CPU)
│   ├── ✅ sync.Pool (buffers JPEG recyclés)
│   ├── ✅ Resize BiLinear (ratio préservé, max 1920×1080)
│   ├── ✅ Watermark adaptatif (couleur auto fond clair/sombre)
│   └── ✅ Position dynamique (4 coins via formulaire)
│
└── Front (React)
    ├── ✅ Drag & drop
    ├── ✅ Slider avant/après
    ├── ✅ Pipeline visualisé (latence par étape)
    └── ✅ Paramètres watermark (texte + position)
```

Ce tableau montre où on en est. Tout ce qui suit est ce qui manque.

---

<a name="observabilite"></a>
## 2. Observabilité — voir ce qui se passe

> **Principe :** on ne peut pas optimiser ce qu'on ne mesure pas.
> Sans observabilité, on optimise à l'aveugle et on rate les vrais bottlenecks.

---

### 2.1 pprof — profiling CPU et mémoire

**C'est quoi :** un outil intégré à Go qui enregistre où le CPU passe son temps et ce qui est alloué en mémoire.

**Ce qu'on apprendrait :**
- Flame graphs (visualiser la pile d'appels)
- Heap profiling (trouver les allocations qui font pression sur le GC)
- Goroutine profiling (détecter les goroutines bloquées)
- Mutex profiling (trouver les contensions de locks)

**Comment l'implémenter sur le projet :**

```go
import _ "net/http/pprof"  // l'import suffit à enregistrer les routes

// Exposer sur un port séparé (ne jamais exposer en prod sur le port public)
go func() {
    log.Println(http.ListenAndServe(":6060", nil))
}()
```

```bash
# Analyser le CPU pendant 30 secondes pendant un load test
go tool pprof http://localhost:6060/debug/pprof/profile?seconds=30

# Analyser la heap
go tool pprof http://localhost:6060/debug/pprof/heap

# Voir les goroutines bloquées
go tool pprof http://localhost:6060/debug/pprof/goroutine
```

**Ce qu'on découvrirait probablement :**
- `adaptiveColor` est le bottleneck (boucle pixel par pixel)
- `jpeg.Encode` alloue malgré le sync.Pool si mal utilisé
- Les goroutines RabbitMQ en attente consomment de la mémoire

**Difficulté :** ⭐⭐ — intégration triviale, lecture des flame graphs demande de la pratique

---

### 2.2 Prometheus + Grafana — métriques en temps réel

**C'est quoi :** Prometheus collecte des métriques numériques toutes les N secondes (scraping), Grafana les affiche en dashboards.

**Ce qu'on apprendrait :**
- Les 4 types de métriques : Counter, Gauge, Histogram, Summary
- Le modèle Pull (Prometheus scrape le serveur) vs Push (le serveur envoie)
- Les percentiles : p50, p95, p99 (pourquoi la moyenne ment)
- PromQL — le langage de requête

**Métriques à exposer sur le projet :**

```go
import "github.com/prometheus/client_golang/prometheus"

var (
    // Nombre total d'uploads
    uploadsTotal = prometheus.NewCounterVec(
        prometheus.CounterOpts{Name: "uploads_total"},
        []string{"status"},  // labels: "success", "error", "rabbit"
    )

    // Latence du pipeline par étape
    stepDuration = prometheus.NewHistogramVec(
        prometheus.HistogramOpts{
            Name:    "pipeline_step_duration_seconds",
            Buckets: prometheus.DefBuckets,
        },
        []string{"step"},  // "redis", "minio", "optimizer", "gzip"
    )

    // Taux de cache hit Redis
    cacheHits = prometheus.NewCounter(
        prometheus.CounterOpts{Name: "redis_cache_hits_total"},
    )

    // Taille des images en entrée/sortie
    imageSize = prometheus.NewHistogramVec(
        prometheus.HistogramOpts{
            Name:    "image_size_bytes",
            Buckets: prometheus.ExponentialBuckets(1024, 2, 20),
        },
        []string{"type"},  // "input", "output"
    )
)
```

**Dashboard Grafana qu'on pourrait construire :**

```
┌─────────────────────────────────────────────────────────────┐
│  NWS Watermark — Performance Dashboard                      │
├──────────────┬──────────────┬──────────────┬────────────────┤
│ Uploads/sec  │ Cache hit %  │ p99 latency  │ Queue depth    │
│    42 req/s  │    87%       │    1.2s      │   RabbitMQ: 3  │
├──────────────┴──────────────┴──────────────┴────────────────┤
│                   Latence par étape (p95)                   │
│  Redis   ████░░░░░░  2ms                                    │
│  MinIO   ███████░░░  45ms                                   │
│  Optim.  ██████████  320ms   ← bottleneck                   │
│  gzip    █░░░░░░░░░  1ms                                    │
└─────────────────────────────────────────────────────────────┘
```

**Difficulté :** ⭐⭐⭐ — intégration Go facile, design des métriques utiles demande de l'expérience

---

### 2.3 OpenTelemetry — tracing distribué

**C'est quoi :** suivre une requête à travers plusieurs services avec un identifiant unique (trace ID). Chaque opération = un "span" avec sa durée.

**Ce qu'on apprendrait :**
- Propagation de contexte entre services (headers W3C TraceContext)
- Corrélation logs + traces
- Détecter les bottlenecks dans une chaîne de microservices
- Jaeger ou Tempo comme backend de visualisation

**Ce qu'on verrait pour un upload :**

```
Trace ID: abc123
│
├── [API] handleUpload              0ms → 380ms
│   ├── [API] readImage             0ms → 2ms
│   ├── [API] sha256                2ms → 3ms
│   ├── [API] redis.Get             3ms → 5ms    ← MISS
│   ├── [API] minio.Put             5ms → 52ms   ← lent !
│   ├── [API] sendToOptimizer       52ms → 370ms
│   │   └── [OPTIMIZER] handleOptimize  0ms → 315ms
│   │       ├── decode              0ms → 8ms
│   │       ├── resize              8ms → 45ms
│   │       ├── watermark           45ms → 312ms  ← TRÈS lent
│   │       └── encode              312ms → 315ms
│   └── [API] redis.Set             370ms → 372ms
└── [API] gzip + send               372ms → 380ms
```

D'un coup on voit que `watermark` prend 267ms sur 380ms totaux.

**Difficulté :** ⭐⭐⭐⭐ — setup complexe, mais c'est le standard industrie

---

### 2.4 Structured logging — logs exploitables

**C'est quoi :** remplacer `log.Printf("[API] ...")` par du JSON structuré avec des champs typés.

**Le problème avec les logs actuels :**

```
[API] ⑤ Optimizer  : ✓ 245.3 KB reçus en 312ms
```

Ce log est lisible mais **pas requêtable** — impossible de filtrer "tous les uploads > 500ms" ou "toutes les erreurs MinIO".

**Avec zerolog :**

```go
log.Info().
    Str("step", "optimizer").
    Int("bytes", len(result)).
    Dur("duration", optimizerDur).
    Str("filename", header.Filename).
    Msg("optimizer response received")
```

**Ce que ça produit :**

```json
{"level":"info","step":"optimizer","bytes":251187,"duration_ms":312,"filename":"photo.jpg","time":"2026-02-24T10:23:41Z","message":"optimizer response received"}
```

Requêtable dans Loki, Elasticsearch, Datadog, etc.

**Difficulté :** ⭐⭐ — migration mécanique mais disciplinée

---

### 2.5 Pyroscope — continuous profiling

**C'est quoi :** pprof en continu, toujours actif en prod, avec historique. On peut comparer "avant deploy" vs "après deploy".

**Ce qu'on apprendrait :** comment les performances évoluent dans le temps, détecter des régressions automatiquement.

**Difficulté :** ⭐⭐ — agent à ajouter, dashboard fourni

---

<a name="resilience"></a>
## 3. Résilience — ne pas tomber sous charge

> **Principe :** un système haute performance doit dégrader gracieusement,
> pas s'effondrer brutalement.

---

### 3.1 Circuit Breaker

**C'est quoi :** si l'optimizer échoue N fois consécutives, on "ouvre le circuit" — on arrête d'essayer et on bascule directement sur RabbitMQ sans attendre le timeout HTTP.

**Les 3 états :**

```
CLOSED (normal)
  │  5 erreurs consécutives
  ▼
OPEN (court-circuit)
  │  après 30 secondes
  ▼
HALF-OPEN (test)
  │  1 requête test :
  │  ✅ succès → retour CLOSED
  └─ ❌ échec  → retour OPEN
```

**Sans circuit breaker :**
```
100 requêtes simultanées → chacune attend 30s timeout → 100 goroutines bloquées
→ mémoire épuisée → API crashe
```

**Avec circuit breaker :**
```
100 requêtes simultanées → les 5 premières échouent → circuit ouvert
→ les 95 suivantes → RabbitMQ immédiatement → 0ms de blocage
```

```go
// Librairie : github.com/sony/gobreaker
cb := gobreaker.NewCircuitBreaker(gobreaker.Settings{
    MaxRequests:  1,              // 1 requête test en half-open
    Interval:     10 * time.Second,
    Timeout:      30 * time.Second,
    ReadyToTrip:  func(counts gobreaker.Counts) bool {
        return counts.ConsecutiveFailures >= 5
    },
})

result, err := cb.Execute(func() (interface{}, error) {
    return sendToOptimizer(url, filename, data, wmText, wmPosition)
})
```

**Ce qu'on apprendrait :** patterns de résilience, fail-fast, bulkhead, le livre "Release It!" de Michael Nygard.

**Difficulté :** ⭐⭐⭐

---

### 3.2 Rate Limiting — limiter les abus

**C'est quoi :** limiter le nombre de requêtes par IP (ou par token) pour éviter qu'un client monopolise les ressources.

**Les deux algorithmes principaux :**

**Token Bucket** (notre cas idéal) :
```
Bucket de 10 tokens, se remplit à 2 tokens/sec
→ bursts autorisés jusqu'à 10 requêtes instantanées
→ débit moyen limité à 2 req/sec
```

**Sliding Window** :
```
Max 100 requêtes sur les 60 dernières secondes glissantes
→ pas de burst autorisé
→ plus équitable entre clients
```

```go
import "golang.org/x/time/rate"

// Map IP → limiteur (avec nettoyage périodique pour éviter les fuites mémoire)
var limiters sync.Map

func getLimiter(ip string) *rate.Limiter {
    if v, ok := limiters.Load(ip); ok {
        return v.(*rate.Limiter)
    }
    // 2 req/sec, burst de 10
    l := rate.NewLimiter(rate.Limit(2), 10)
    limiters.Store(ip, l)
    return l
}

func rateLimitMiddleware(next http.Handler) http.Handler {
    return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        limiter := getLimiter(r.RemoteAddr)
        if !limiter.Allow() {
            http.Error(w, "Too Many Requests", http.StatusTooManyRequests)
            return
        }
        next.ServeHTTP(w, r)
    })
}
```

**Ce qu'on apprendrait :** Token Bucket, Leaky Bucket, Sliding Window Counter, `sync.Map`, nettoyage de state (TTL sur les entrées).

**Difficulté :** ⭐⭐

---

### 3.3 Retry avec backoff exponentiel + jitter

**Le problème du retry fixe actuel :**

```go
// ❌ Actuel — retry fixe
time.Sleep(10 * time.Second)
```

Si 100 jobs échouent en même temps, ils réessaient **tous** dans 10 secondes → pic de charge → ils rééchouent tous → ...

**Backoff exponentiel + jitter :**

```go
// ✅ Exponential backoff avec jitter
func backoff(attempt int) time.Duration {
    base := time.Second
    max  := 5 * time.Minute
    exp  := base * (1 << attempt)  // 1s, 2s, 4s, 8s, 16s...
    if exp > max { exp = max }

    // Jitter : +/- 25% aléatoire → les retries se dispersent dans le temps
    jitter := time.Duration(rand.Int63n(int64(exp / 4)))
    return exp + jitter
}

// Attempt 0 :  1s  ± 250ms
// Attempt 1 :  2s  ± 500ms
// Attempt 2 :  4s  ± 1s
// Attempt 3 :  8s  ± 2s
// Attempt 4 : 16s  ± 4s
```

**Ce qu'on apprendrait :** Thundering Herd problem, jitter, les patterns AWS de retry (https://aws.amazon.com/blogs/architecture/exponential-backoff-and-jitter/).

**Difficulté :** ⭐⭐

---

### 3.4 Context + timeout propagation

**C'est quoi :** passer un `context.Context` à travers tout le pipeline pour que si le client coupe la connexion, toutes les opérations en cours s'annulent.

**Actuellement :** si le client ferme la connexion pendant le traitement, l'API continue quand même à travailler (résultat stocké dans Redis pour rien).

```go
// Avec context propagé
func handleUpload(w http.ResponseWriter, r *http.Request) {
    ctx := r.Context()  // annulé si le client déconnecte

    // Redis respecte le contexte
    cached, err := redisClient.Get(ctx, cacheKey).Bytes()

    // MinIO respecte le contexte
    _, err = minioClient.PutObject(ctx, bucket, key, ...)

    // Timeout sur l'optimizer : max 10s
    ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
    defer cancel()
    result, err := sendToOptimizer(ctx, url, filename, data, ...)
}
```

**Ce qu'on apprendrait :** `context.Context`, `context.WithTimeout`, `context.WithCancel`, `context.WithDeadline`, propagation dans les goroutines, annulation en cascade.

**Difficulté :** ⭐⭐⭐ — conceptuellement simple, bien faire la propagation est subtil

---

### 3.5 Health checks + Readiness probes

**C'est quoi :** des endpoints qui disent si le service est vivant (`/health`) et prêt à recevoir du trafic (`/ready`).

```go
// /health — le processus est-il vivant ?
mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
    w.WriteHeader(http.StatusOK)
})

// /ready — toutes les dépendances sont-elles disponibles ?
mux.HandleFunc("GET /ready", func(w http.ResponseWriter, r *http.Request) {
    checks := map[string]string{}

    if err := redisClient.Ping(r.Context()).Err(); err != nil {
        checks["redis"] = err.Error()
    }
    if _, err := minioClient.BucketExists(r.Context(), minioBucket); err != nil {
        checks["minio"] = err.Error()
    }

    if len(checks) > 0 {
        w.WriteHeader(http.StatusServiceUnavailable)
        json.NewEncoder(w).Encode(checks)
        return
    }
    w.WriteHeader(http.StatusOK)
})
```

**Ce qu'on apprendrait :** Kubernetes liveness/readiness/startup probes, graceful shutdown, load balancer integration.

**Difficulté :** ⭐⭐

---

### 3.6 Graceful shutdown

**C'est quoi :** quand le processus reçoit SIGTERM (ex: `docker stop`), finir les requêtes en cours avant de s'arrêter.

```go
srv := &http.Server{Addr: ":3000", Handler: corsMiddleware(mux)}

go srv.ListenAndServe()

// Attendre SIGTERM ou SIGINT
quit := make(chan os.Signal, 1)
signal.Notify(quit, syscall.SIGTERM, syscall.SIGINT)
<-quit

// 30 secondes pour finir les requêtes en cours
ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
defer cancel()
srv.Shutdown(ctx)
```

**Ce qu'on apprendrait :** signaux Unix, lifecycle d'un processus, rolling deployments sans coupure.

**Difficulté :** ⭐⭐

---

<a name="go-perf"></a>
## 4. Performance Go — le runtime en profondeur

> **Principe :** Go fait beaucoup de choses automatiquement (GC, scheduler).
> Comprendre ses mécanismes permet de travailler avec lui plutôt que contre lui.

---

### 4.1 Le scheduler Go — goroutines et GOMAXPROCS

**C'est quoi :** Go utilise un scheduler M:N — N goroutines tournent sur M threads OS. Le scheduler Go décide quelle goroutine tourne sur quel thread.

```
Goroutines (N)     : 10 000 goroutines créées
Threads OS (M)     : GOMAXPROCS threads (= nb de CPU par défaut)
Processeurs (P)    : file de run queue par thread

G = Goroutine  M = Thread OS  P = Processeur logique

   P1          P2          P3          P4
[ G1 G3 G7 ][ G2 G8 ]  [ G4 G9 ]  [ G5 G6 ]
     │              │          │          │
     M1             M2         M3         M4
     │              │          │          │
     └──────────── CPU ────────────────────┘
```

**Work stealing :** si P1 est vide et P2 a 8 goroutines → P1 "vole" la moitié du travail de P2.

**Ce qu'on apprendrait :** pourquoi `GOMAXPROCS=1` peut être plus rapide pour certains workloads, preemption coopérative vs asynchrone, `runtime.Gosched()`.

**Difficulté :** ⭐⭐⭐

---

### 4.2 Le garbage collector Go — GC tuning

**C'est quoi :** Go utilise un GC concurrent tri-color mark-and-sweep. Il tourne en parallèle du programme et cause des "stop-the-world" très courts.

**Le paramètre GOGC :**

```bash
# GOGC=100 (défaut) : GC se déclenche quand la heap double
# GOGC=200          : GC moins fréquent → moins de CPU GC, plus de mémoire
# GOGC=50           : GC plus fréquent → plus de CPU GC, moins de mémoire
# GOGC=off          : désactive le GC (dangereux)

GOGC=200 ./optimizer  # bon pour un service avec beaucoup d'allocations temporaires
```

**GOMEMLIMIT (Go 1.19+) :**

```bash
# Limite la mémoire totale utilisée — le GC devient plus agressif si on approche la limite
GOMEMLIMIT=512MiB ./api
```

**Le memory ballast trick (pré-Go 1.19) :**

```go
// Allouer un gros tableau vide pour tromper le GC et le rendre moins fréquent
// Le GC croit avoir plus de mémoire disponible
ballast := make([]byte, 100*1024*1024)  // 100 MB "fantôme"
runtime.KeepAlive(ballast)
```

**Ce qu'on apprendrait :** tri-color mark-and-sweep, write barriers, GC pause times, GOGC, GOMEMLIMIT, le balance CPU/mémoire.

**Difficulté :** ⭐⭐⭐⭐

---

### 4.3 Escape analysis — allouer sur la stack vs heap

**C'est quoi :** le compilateur Go décide si une variable va sur la **stack** (rapide, pas de GC) ou la **heap** (plus lent, GC doit la collecter).

```go
// Stack allocation (pas de GC)
func add(a, b int) int {
    result := a + b  // result reste sur la stack
    return result
}

// Heap allocation (GC doit collecter)
func newSlice() []byte {
    s := make([]byte, 1024)  // s "échappe" vers la heap si retourné
    return s
}
```

**Voir ce que le compilateur décide :**

```bash
go build -gcflags="-m" ./optimizer/...

# Output :
# ./main.go:164:13: image.NewRGBA(img.Bounds()) escapes to heap
# ./main.go:201:14: make([]byte, sampleW*sampleH) does not escape
```

**Ce qu'on apprendrait :** `//go:noescape`, inlining (`//go:noinline`), bounds check elimination, les micro-optimisations valides vs prématurées.

**Difficulté :** ⭐⭐⭐⭐

---

### 4.4 sync primitives avancées

**`sync.Map`** — map thread-safe sans mutex global :
```go
// Meilleur que map + RWMutex quand les clés sont stables (beaucoup de lectures, peu d'écritures)
var cache sync.Map
cache.Store("key", value)
v, ok := cache.Load("key")
cache.LoadOrStore("key", defaultValue)
```

**`atomic`** — opérations sans mutex pour les compteurs :
```go
import "sync/atomic"

var requestCount int64
atomic.AddInt64(&requestCount, 1)       // incrémentation atomique
n := atomic.LoadInt64(&requestCount)    // lecture atomique
```

**`errgroup`** — goroutines parallèles avec gestion d'erreur :
```go
import "golang.org/x/sync/errgroup"

// Lancer resize ET charger la font en parallèle
g, ctx := errgroup.WithContext(context.Background())

var resized image.Image
g.Go(func() error {
    resized = resize(img)
    return nil
})

var metadata map[string]string
g.Go(func() error {
    metadata = extractMetadata(img)
    return nil
})

if err := g.Wait(); err != nil {
    return err  // retourne la première erreur
}
```

**Difficulté :** ⭐⭐⭐

---

### 4.5 Channel patterns avancés

**Fan-out** (distribuer le travail sur N workers) :
```
Source ──► ch ──┬──► Worker 1
                ├──► Worker 2
                └──► Worker 3
```

**Fan-in** (agréger N résultats en un) :
```
Worker 1 ──┐
Worker 2 ──┼──► merge ch ──► Consumer
Worker 3 ──┘
```

**Pipeline** (chaîner des étapes) :
```
decode(images) ──► resize(decoded) ──► watermark(resized) ──► encode(watermarked)
```

**Ce qu'on apprendrait :** `done` channel pattern, `select` avec timeout, channel directionality (`chan<-` vs `<-chan`), les patterns du livre "Concurrency in Go" de Katherine Cox-Buday.

**Difficulté :** ⭐⭐⭐

---

<a name="cache"></a>
## 5. Cache avancé — aller plus loin que Redis

---

### 5.1 Cache multi-niveaux (L1 + L2)

**Principe :**

```
Requête
  │
  ▼
L1 : cache en RAM du process (ristretto / groupcache)
  │  ~1µs    hit rate: 40%
  ▼
L2 : Redis
  │  ~1ms    hit rate: 85%
  ▼
L3 : traitement complet (optimizer)
     ~300ms  hit rate: 0% (cache miss total)
```

**Ristretto** (cache LRU concurrent de DGraph) :

```go
cache, _ := ristretto.NewCache(&ristretto.Config{
    NumCounters: 1e7,      // 10M compteurs de fréquence
    MaxCost:     1 << 30,  // 1 GB max
    BufferItems: 64,
})

// Stocker (cost = taille en bytes)
cache.Set(cacheKey, imageBytes, int64(len(imageBytes)))

// Lire
if val, found := cache.Get(cacheKey); found {
    return val.([]byte), nil
}
```

**Ce qu'on apprendrait :** LRU vs LFU vs ARC eviction, cache coherence, invalidation (le problème le plus dur de l'informatique), TinyLFU algorithm de ristretto.

**Difficulté :** ⭐⭐⭐

---

### 5.2 Cache stampede — le problème de l'expiration simultanée

**C'est quoi :** 1000 requêtes arrivent au même moment, le cache expire → les 1000 font le traitement en même temps → le service explose.

```
T=0   : 1000 requêtes → cache HIT (Redis)
T=24h : le cache expire
T=24h+1ms : 1000 requêtes → cache MISS → 1000 fois l'optimizer → 💥
```

**Solution : singleflight** — si 1000 requêtes veulent la même clé, une seule fait le travail, les 999 autres attendent le résultat.

```go
import "golang.org/x/sync/singleflight"

var sf singleflight.Group

result, err, shared := sf.Do(cacheKey, func() (interface{}, error) {
    // Ce code n'est exécuté qu'UNE SEULE FOIS même si 1000 goroutines arrivent ici
    return processImage(data, wmText, wmPosition)
})

if shared {
    log.Printf("résultat partagé avec d'autres goroutines")
}
```

**Ce qu'on apprendrait :** singleflight, mutex coarse-grained vs fine-grained, probabilistic early expiration.

**Difficulté :** ⭐⭐⭐

---

### 5.3 ETags + cache HTTP côté client

**C'est quoi :** envoyer un identifiant de version au client. Si l'image n'a pas changé, répondre `304 Not Modified` sans retransférer les données.

```
Requête 1 :
  Client → GET /image/abc123
  Serveur → 200 + image + ETag: "abc123"

Requête 2 (même image) :
  Client → GET /image/abc123 + If-None-Match: "abc123"
  Serveur → 304 Not Modified (0 bytes transférés)
```

```go
func handleGetImage(w http.ResponseWriter, r *http.Request) {
    hash := r.PathValue("hash")
    etag := `"` + hash + `"`

    // Si le client a déjà cette version → 304
    if r.Header.Get("If-None-Match") == etag {
        w.WriteHeader(http.StatusNotModified)
        return
    }

    data, _ := redisClient.Get(r.Context(), hash).Bytes()
    w.Header().Set("ETag", etag)
    w.Header().Set("Cache-Control", "public, max-age=86400")
    sendResponse(w, r, data)
}
```

**Ce qu'on apprendrait :** HTTP caching headers (`ETag`, `Last-Modified`, `Cache-Control`, `Vary`), strong vs weak ETags, conditional requests.

**Difficulté :** ⭐⭐

---

### 5.4 Bloom filter — éviter les requêtes inutiles

**C'est quoi :** structure de données probabiliste qui répond "cet élément n'existe PAS" avec certitude, ou "cet élément EXISTE probablement". Zéro faux négatifs, quelques faux positifs contrôlés.

**Usage sur le projet :** avant de faire un Redis.Get, vérifier dans le Bloom filter si l'image a déjà été traitée. Si le Bloom filter dit "non" → pas la peine d'interroger Redis.

```
Bloom filter → "probablement oui" → Redis.Get → HIT ou MISS
Bloom filter → "définitivement non" → aller directement à l'optimizer
```

```
Taille : 1 million d'entrées → ~1.2 MB de RAM
Faux positifs : ~1%
Gain : évite ~99% des Redis.Get inutiles
```

**Ce qu'on apprendrait :** hash functions, bit arrays, taux de faux positifs, HyperLogLog (compter des éléments uniques approximativement), Count-Min Sketch.

**Difficulté :** ⭐⭐⭐⭐

---

<a name="http"></a>
## 6. HTTP avancé — protocoles modernes

---

### 6.1 HTTP/2 — multiplexing et compression de headers

**Le problème HTTP/1.1 :**

```
HTTP/1.1 : une requête à la fois par connexion
→ le navigateur ouvre 6-8 connexions TCP en parallèle pour contourner
→ overhead de connexion, head-of-line blocking
```

**HTTP/2 :**

```
Une seule connexion TCP
  │
  ├── Stream 1 : GET /upload    ──►  traitement parallèle
  ├── Stream 2 : GET /status    ──►  traitement parallèle
  └── Stream 3 : GET /image     ──►  traitement parallèle
```

**HPACK** : compression des headers HTTP/2. Les headers répétés (User-Agent, Accept-Encoding...) ne sont envoyés qu'une fois puis indexés.

```go
// En Go, HTTP/2 est automatique avec TLS
srv := &http.Server{
    Addr:    ":443",
    Handler: mux,
    TLSConfig: &tls.Config{
        MinVersion: tls.VersionTLS13,
    },
}
// http2.ConfigureServer(srv, nil) — automatique avec TLS
srv.ListenAndServeTLS("cert.pem", "key.pem")
```

**Ce qu'on apprendrait :** streams, frames, flow control, server push (HTTP/2), HPACK compression, head-of-line blocking.

**Difficulté :** ⭐⭐⭐

---

### 6.2 HTTP/3 — QUIC remplace TCP

**Le problème de TCP :**
- TCP reordering : si un paquet est perdu, tous les streams HTTP/2 bloquent (head-of-line blocking au niveau TCP)
- Handshake lent : 1-3 RTT pour établir TLS sur TCP

**QUIC (HTTP/3) :**

```
UDP + QUIC = streams indépendants
→ si un paquet est perdu, seul le stream concerné est retardé
→ 0-RTT reconnexion pour les clients connus
```

```
HTTP/1.1 :  TCP(3-way) + TLS(2-way) = 3 RTT avant la première donnée
HTTP/2   :  TCP(3-way) + TLS(2-way) = 3 RTT (même connexion ensuite)
HTTP/3   :  QUIC = 1 RTT (ou 0-RTT pour clients connus)
```

**Ce qu'on apprendrait :** UDP vs TCP, QUIC protocol, 0-RTT, `quic-go` library.

**Difficulté :** ⭐⭐⭐⭐⭐

---

### 6.3 gRPC — performance vs REST

**C'est quoi :** protocole RPC (Remote Procedure Call) développé par Google. Utilise Protocol Buffers (binaire) au lieu de JSON (texte) et HTTP/2 au lieu de HTTP/1.1.

**Comparaison avec notre API REST :**

| | REST + JSON (actuel) | gRPC + Protobuf |
|---|---|---|
| Format | JSON (texte) | Protobuf (binaire) |
| Taille payload | 100% | ~30-50% |
| Parse CPU | Moyen | Minimal |
| Streaming | Limité | Natif (bidirectionnel) |
| Code generation | Non | Oui (`.proto` → Go) |
| Browser support | Natif | Nécessite grpc-web |

**Pour la communication API → Optimizer :** gRPC serait plus efficace que multipart HTTP.

```protobuf
// optimize.proto
service Optimizer {
    rpc Optimize(OptimizeRequest) returns (OptimizeResponse);
}

message OptimizeRequest {
    bytes  image_data  = 1;
    string wm_text     = 2;
    string wm_position = 3;
    string filename    = 4;
}

message OptimizeResponse {
    bytes result = 1;
}
```

**Ce qu'on apprendrait :** Protocol Buffers, IDL (Interface Definition Language), code generation, streaming RPC, interceptors (équivalent des middlewares HTTP).

**Difficulté :** ⭐⭐⭐⭐

---

### 6.4 Server-Sent Events — remplacer le polling

**Le problème actuel :** quand l'optimizer est KO, le front poll `/status/{hash}` toutes les 500ms. C'est du gaspillage — on fait des requêtes HTTP même quand rien n'a changé.

**Server-Sent Events (SSE) :** le serveur pousse les mises à jour au client dès qu'elles arrivent.

```go
func handleStatusSSE(w http.ResponseWriter, r *http.Request) {
    hash := r.PathValue("hash")

    w.Header().Set("Content-Type", "text/event-stream")
    w.Header().Set("Cache-Control", "no-cache")

    flusher := w.(http.Flusher)

    for {
        select {
        case <-r.Context().Done():
            return  // client déconnecté
        case <-time.After(500 * time.Millisecond):
            exists, _ := redisClient.Exists(r.Context(), hash).Result()
            if exists == 1 {
                fmt.Fprintf(w, "data: {\"status\":\"done\",\"url\":\"/image/%s\"}\n\n", hash)
                flusher.Flush()
                return
            }
            fmt.Fprintf(w, "data: {\"status\":\"pending\"}\n\n")
            flusher.Flush()
        }
    }
}
```

**Ce qu'on apprendrait :** SSE vs WebSockets vs polling, `http.Flusher`, long-polling, push vs pull.

**Difficulté :** ⭐⭐

---

<a name="image"></a>
## 7. Optimisation image — formats et parallélisme

---

### 7.1 WebP et AVIF — les formats modernes

**Comparaison pour une photo de 500 KB en JPEG qualité 85 :**

| Format | Taille | Qualité visuelle | Support navigateur |
|---|---|---|---|
| JPEG | 500 KB | Référence | 100% |
| WebP | ~350 KB (-30%) | Équivalente | 97% |
| AVIF | ~250 KB (-50%) | Légèrement meilleure | 90% |
| JPEG XL | ~200 KB (-60%) | Meilleure | 75% |

**Négociation via Accept :**

```go
// Le navigateur annonce ce qu'il accepte
// Accept: image/avif,image/webp,image/jpeg,*/*

func bestFormat(r *http.Request) string {
    accept := r.Header.Get("Accept")
    if strings.Contains(accept, "image/avif") { return "avif" }
    if strings.Contains(accept, "image/webp") { return "webp" }
    return "jpeg"
}
```

**Ce qu'on apprendrait :** codecs d'image modernes, DCT vs AV1 (AVIF), librairies Go `golang.org/x/image/webp`, `gen2brain/avif`.

**Difficulté :** ⭐⭐⭐

---

### 7.2 Traitement parallèle des pixels

**Le bottleneck actuel de `sampleLuminance` :**

```go
// ❌ Séquentiel — 200×50 = 10 000 pixels, un par un
for py := startY; py < endY; py++ {
    for px := startX; px < endX; px++ {
        r, g, b, _ := img.At(px, py).RGBA()
        total += 0.299*float64(r>>8) + ...
    }
}
```

**Parallélisation par lignes :**

```go
// ✅ Parallèle — chaque ligne traitée par une goroutine
var mu sync.Mutex
var total float64

var wg sync.WaitGroup
for py := startY; py < endY; py++ {
    wg.Add(1)
    go func(row int) {
        defer wg.Done()
        var rowTotal float64
        for px := startX; px < endX; px++ {
            r, g, b, _ := img.At(px, row).RGBA()
            rowTotal += 0.299*float64(r>>8) + 0.587*float64(g>>8) + 0.114*float64(b>>8)
        }
        mu.Lock()
        total += rowTotal
        mu.Unlock()
    }(py)
}
wg.Wait()
```

**Ce qu'on apprendrait :** quand la parallélisation aide vs quand elle nuit (overhead goroutines vs gain calcul), false sharing, SIMD/AVX.

**Difficulté :** ⭐⭐⭐

---

### 7.3 Progressive JPEG et lazy decoding

**Progressive JPEG :** l'image s'affiche d'abord floue puis de plus en plus nette (comme sur les sites web lents).

```go
// Le package standard image/jpeg ne supporte pas le progressive JPEG en écriture
// Il faut libjpeg-turbo via cgo ou une librairie externe
```

**Lazy decoding :** ne décoder que le header de l'image (dimensions, format) sans décoder les pixels — utile pour valider une image sans la charger entièrement.

```go
// Lire uniquement la config (largeur, hauteur, format) sans décoder les pixels
config, format, err := image.DecodeConfig(file)
fmt.Printf("%s : %dx%d\n", format, config.Width, config.Height)
```

**Ce qu'on apprendrait :** structure interne des formats JPEG/PNG/WebP, libjpeg-turbo, cgo (appeler du C depuis Go).

**Difficulté :** ⭐⭐⭐⭐

---

<a name="distribue"></a>
## 8. Architecture distribuée — scaler horizontalement

---

### 8.1 Scaling horizontal de l'optimizer

**Actuel :** 1 instance de l'optimizer, semaphore limité à nb_CPU.

**Scalé :** 3 instances derrière un load balancer.

```yaml
# docker-compose.yml
optimizer:
  build: ./optimizer
  deploy:
    replicas: 3          # 3 instances

nginx:
  image: nginx:alpine
  config: |
    upstream optimizer {
        least_conn;              # envoyer au moins chargé
        server optimizer:3001;   # Docker résout en round-robin
    }
```

**Ce qu'on apprendrait :** round-robin vs least-connections vs IP hash, health checks du load balancer, sticky sessions (et pourquoi les éviter), service discovery.

**Difficulté :** ⭐⭐⭐

---

### 8.2 Dead Letter Queue (DLQ)

**Actuel :** si un job RabbitMQ échoue indéfiniment → NACK → requeue → boucle infinie.

**Avec DLQ :** après 3 NACKs → le message va dans `watermark_failed` au lieu de boucler.

```go
// Déclarer la queue principale avec DLQ attachée
args := amqp.Table{
    "x-dead-letter-exchange":    "",                  // exchange par défaut
    "x-dead-letter-routing-key": "watermark_failed",  // queue DLQ
    "x-message-ttl":             int64(24 * 60 * 60 * 1000), // 24h max
}
ch.QueueDeclare("watermark_retry", true, false, false, false, args)

// Déclarer la DLQ (passive, juste pour stocker)
ch.QueueDeclare("watermark_failed", true, false, false, false, nil)
```

**Ce qu'on apprendrait :** DLQ patterns, message replay, poison pills, observabilité des queues (RabbitMQ Management UI).

**Difficulté :** ⭐⭐⭐

---

### 8.3 Consistent hashing — Redis Cluster

**Le problème du sharding naïf :**

```
hash % 3 noeuds = noeud cible

Si on passe de 3 à 4 noeuds → hash % 4 → TOUS les mappings changent
→ 100% de cache miss pendant des heures
```

**Consistent hashing :**

```
Anneau de 0 à 2^32
Chaque noeud occupe plusieurs positions sur l'anneau
Chaque clé → position sur l'anneau → noeud le plus proche

Ajouter un noeud → seules les clés entre lui et son voisin migrent (~1/N keys)
```

**Ce qu'on apprendrait :** Redis Cluster (16384 hash slots), consistent hashing, virtual nodes, rendezvous hashing.

**Difficulté :** ⭐⭐⭐⭐

---

### 8.4 Event Sourcing + CQRS

**CQRS (Command Query Responsibility Segregation) :**
- Séparer les opérations d'écriture (Commands) des lectures (Queries)
- Sur le projet : la command = upload+watermark, la query = récupérer le résultat

**Event Sourcing :**
- Au lieu de stocker l'état final → stocker tous les événements qui l'ont produit
- `ImageUploaded{hash, filename}` → `WatermarkApplied{hash, text, position}` → `ImageServed{hash}`
- On peut rejouer l'historique, auditer, faire du time-travel debugging

**Ce qu'on apprendrait :** immutabilité des événements, projections, sagas, Kafka pour la persistance des événements.

**Difficulté :** ⭐⭐⭐⭐⭐

---

<a name="linux"></a>
## 9. Linux & OS — le niveau zéro

> **Principe :** Go compile vers du code natif qui appelle directement le kernel Linux.
> Comprendre les syscalls et les primitives OS explique pourquoi certaines optimisations fonctionnent.

---

### 9.1 io_uring — I/O asynchrone Linux

**C'est quoi :** interface kernel Linux (depuis 5.1) pour faire des I/O asynchrones sans syscalls par opération. Au lieu de `read()` + `write()` bloquants, on soumet un batch d'opérations dans un ring buffer partagé.

```
Avant (epoll) :           Après (io_uring) :
  read() → syscall          submit batch → 1 syscall pour N opérations
  poll()  → syscall         poll()        → peut être remplacé par busy-wait
  write() → syscall         → 0 copie mémoire user/kernel
```

**Ce qu'on apprendrait :** ring buffers, zero-copy I/O, uring_enter syscall, différence epoll/kqueue/IOCP, pourquoi Node.js et nginx sont rapides.

**Difficulté :** ⭐⭐⭐⭐⭐

---

### 9.2 Zero-copy — sendfile et splice

**Le problème de la copie classique :**

```
Disque → kernel buffer → user buffer → kernel buffer réseau → NIC
            (copie 1)    (copie 2)       (copie 3)
```

**`sendfile` syscall (zero-copy) :**

```
Disque → kernel buffer ──────────────────────────────────────► NIC
                                    (0 copie user space)
```

**En Go :**

```go
// Go utilise sendfile automatiquement quand on fait :
src, _ := os.Open("image.jpg")
dst, _ := os.Create("output.jpg")
io.Copy(dst, src)  // → utilise sendfile sous le capot sur Linux
```

Mais dès qu'on passe par un `http.ResponseWriter`, on repasse en copie normale. Des librairies comme `fasthttp` évitent ça.

**Ce qu'on apprendrait :** sendfile(2), splice(2), mmap, DMA, le chemin d'une donnée du disque au réseau.

**Difficulté :** ⭐⭐⭐⭐⭐

---

### 9.3 epoll — le cœur des serveurs haute performance

**C'est quoi :** mécanisme Linux pour surveiller des milliers de file descriptors (connexions réseau) avec un seul appel bloquant.

```
Sans epoll (select/poll) :
  for each connection : is_ready() → O(n) par itération → inutilisable à 10k connexions

Avec epoll :
  kernel notifie "ces X connexions sont prêtes" → O(1) par notification
  → c'est ce qui permet à nginx de gérer 1M connexions simultanées
```

Go utilise epoll en interne pour son scheduler réseau. Comprendre epoll explique pourquoi les goroutines Go sont si légères comparées aux threads.

**Ce qu'on apprendrait :** le C10K problem, edge-triggered vs level-triggered, kqueue (macOS), IOCP (Windows), netpoller de Go.

**Difficulté :** ⭐⭐⭐⭐⭐

---

<a name="load-testing"></a>
## 10. Load testing — mesurer avant d'optimiser

> **Principe :** sans load test, toutes les optimisations sont des suppositions.

---

### 10.1 k6 — load testing moderne

**C'est quoi :** outil de load testing scriptable en JavaScript, développé par Grafana Labs.

```javascript
// load-test.js
import http from 'k6/http';

export let options = {
    stages: [
        { duration: '30s', target: 10  },   // montée à 10 users
        { duration: '1m',  target: 50  },   // 50 users pendant 1 min
        { duration: '30s', target: 100 },   // pic à 100 users
        { duration: '30s', target: 0   },   // descente
    ],
    thresholds: {
        http_req_duration: ['p95<500'],  // 95% des requêtes < 500ms
        http_req_failed:   ['rate<0.01'], // < 1% d'erreurs
    },
};

export default function() {
    const formData = {
        image: http.file(open('./test.jpg', 'b'), 'test.jpg', 'image/jpeg'),
        wm_text: 'NWS © 2026',
        wm_position: 'bottom-right',
    };
    const res = http.post('http://localhost:3000/upload', formData);
    check(res, { 'status 200': (r) => r.status === 200 });
}
```

```bash
k6 run --out prometheus=remote_write_url load-test.js
```

**Ce qu'on apprendrait :** Virtual Users (VU), ramp-up, throughput vs latency tradeoff, percentiles (p95/p99), corrélation load test + pprof.

**Difficulté :** ⭐⭐

---

### 10.2 wrk et vegeta — tests simples et rapides

```bash
# wrk — 12 threads, 400 connexions, pendant 30s
wrk -t12 -c400 -d30s http://localhost:3000/image/abc123

# vegeta — 100 req/sec pendant 60s
echo "GET http://localhost:3000/image/abc123" | \
  vegeta attack -rate=100 -duration=60s | \
  vegeta report
```

**Ce qu'on apprendrait :** différence wrk (throughput max) vs vegeta (débit constant), latency distribution, coordinated omission problem (pourquoi les benchmarks mentent souvent).

**Difficulté :** ⭐

---

### 10.3 Coordinated Omission — pourquoi les benchmarks mentent

**C'est quoi :** le problème le plus souvent ignoré dans les benchmarks de performance.

```
Service prend 10ms normalement, mais 10s sous charge

Benchmark naïf :
  Envoie une requête → attend la réponse → envoie la suivante
  → "10ms de latence moyenne !"

Réalité : si 100 requêtes arrivent en 1 seconde mais le service prend 10s
  → 990 requêtes attendent → latence réelle = plusieurs secondes

Le benchmark naïf ne mesure pas l'attente, seulement le traitement.
```

**La solution :** HdrHistogram + scheduled requests (vegeta, wrk2, JMeter).

**Ce qu'on apprendrait :** Gil Tene's talk "How NOT to measure latency", HdrHistogram, latency vs response time.

**Difficulté :** ⭐⭐⭐ (conceptuel, pas de code)

---

<a name="chaos"></a>
## 11. Chaos Engineering — tester les pannes volontairement

> **Principe :** Netflix a inventé Chaos Monkey : un outil qui tue des serveurs aléatoirement en prod.
> Si ton système tient face aux pannes, c'est qu'il est vraiment résilient.

---

### 11.1 Pumba — chaos pour Docker

**C'est quoi :** outil qui injecte des pannes dans des conteneurs Docker (latence, perte de paquets, crash).

```bash
# Ajouter 200ms de latence sur les paquets sortants de l'optimizer
pumba netem --duration 5m delay --time 200 watermark-optimizer-1

# Tuer le conteneur optimizer toutes les 30 secondes
pumba --random --interval 30s kill watermark-optimizer-1

# Perdre 10% des paquets réseau
pumba netem --duration 2m loss --percent 10 watermark-api-1
```

**Ce qu'on découvrirait :**
- Est-ce que le fallback RabbitMQ se déclenche vraiment ?
- Est-ce que le circuit breaker s'ouvre ?
- Est-ce que les logs montrent clairement ce qui se passe ?

**Ce qu'on apprendrait :** Game Days, blast radius, steady state hypothesis, les principes de Chaos Engineering (Principles of Chaos).

**Difficulté :** ⭐⭐⭐

---

### 11.2 Toxiproxy — simuler des réseaux dégradés

**C'est quoi :** proxy TCP qui permet de simuler des conditions réseau dégradées entre services.

```go
// Créer un proxy Redis avec latence aléatoire
client := toxiproxy.NewClient("localhost:8474")
proxy, _ := client.CreateProxy("redis", "localhost:16379", "localhost:6379")

// Ajouter 100ms de latence
proxy.AddToxic("latency", "latency", "downstream", 1.0, toxiproxy.Attributes{
    "latency": 100,
    "jitter":  50,
})

// Simuler une connexion intermittente (down 10% du temps)
proxy.AddToxic("intermittent", "timeout", "downstream", 0.1, toxiproxy.Attributes{
    "timeout": 0,
})
```

**Ce qu'on apprendrait :** tester les timeouts, valider les circuit breakers, reproduction de bugs réseau difficiles à reproduire.

**Difficulté :** ⭐⭐⭐

---

<a name="securite"></a>
## 12. Sécurité — ce qui manque en prod

---

### 12.1 TLS / HTTPS avec autocert

```go
import "golang.org/x/crypto/acme/autocert"

// Let's Encrypt automatique
m := &autocert.Manager{
    Cache:      autocert.DirCache("certs"),
    Prompt:     autocert.AcceptTOS,
    HostPolicy: autocert.HostWhitelist("watermark.example.com"),
}

srv := &http.Server{
    Addr:      ":443",
    TLSConfig: m.TLSConfig(),
    Handler:   mux,
}
srv.ListenAndServeTLS("", "")  // autocert gère les certificats
```

**Ce qu'on apprendrait :** TLS 1.3, ACME protocol, certificate pinning, HSTS, mTLS (mutual TLS pour la communication inter-services).

**Difficulté :** ⭐⭐⭐

---

### 12.2 Validation et sécurité des uploads

**Ce qui manque actuellement :**

```go
// ❌ Pas de validation du type MIME réel
file, header, _ := r.FormFile("image")
// On fait confiance au Content-Type envoyé par le client

// ✅ Lire les magic bytes pour détecter le vrai format
buf := make([]byte, 512)
file.Read(buf)
file.Seek(0, io.SeekStart)

contentType := http.DetectContentType(buf)
if contentType != "image/jpeg" && contentType != "image/png" {
    http.Error(w, "Format non supporté", http.StatusBadRequest)
    return
}

// ✅ Limiter la taille
r.Body = http.MaxBytesReader(w, r.Body, 20*1024*1024)  // 20 MB max
```

**Ce qu'on apprendrait :** magic bytes, MIME sniffing, zip bombs (images malformées qui explosent en décompression), path traversal.

**Difficulté :** ⭐⭐

---

### 12.3 JWT + authentification

```go
import "github.com/golang-jwt/jwt/v5"

func authMiddleware(next http.Handler) http.Handler {
    return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        token := r.Header.Get("Authorization")
        // Bearer <token>
        claims, err := jwt.Parse(token[7:], func(t *jwt.Token) (interface{}, error) {
            return publicKey, nil  // RS256 : vérification avec clé publique
        })
        if err != nil {
            http.Error(w, "Unauthorized", http.StatusUnauthorized)
            return
        }
        ctx := context.WithValue(r.Context(), "user", claims.Subject)
        next.ServeHTTP(w, r.WithContext(ctx))
    })
}
```

**Ce qu'on apprendrait :** RS256 vs HS256, refresh tokens, token revocation, JWKS endpoints.

**Difficulté :** ⭐⭐⭐

---

<a name="ordre"></a>
## 13. Ordre d'apprentissage suggéré

```
PHASE 1 — Mesurer (sans mesure, on optimise à l'aveugle)
├── pprof                → comprendre où va le CPU et la mémoire
├── k6 load test         → établir une baseline de performance
└── structured logging   → rendre les logs exploitables

PHASE 2 — Résilience (rendre le système robuste)
├── context + timeout    → annuler le travail inutile
├── graceful shutdown    → zéro coupure au déploiement
├── rate limiting        → se protéger des abus
└── circuit breaker      → fail-fast sur l'optimizer

PHASE 3 — Observabilité (voir en production)
├── Prometheus + Grafana → dashboards de métriques
├── health checks        → intégration Docker/Kubernetes
└── OpenTelemetry        → tracing distribué end-to-end

PHASE 4 — Performance (optimiser avec des données)
├── Cache L1 (ristretto) → réduire les appels Redis
├── singleflight         → éliminer le cache stampede
├── ETags                → éviter de retransférer les images
└── GC tuning (GOGC)     → réduire la pression mémoire

PHASE 5 — Scaling (passer à l'échelle)
├── HTTP/2               → multiplexing connexions
├── WebP/AVIF            → images plus légères
├── scaling horizontal   → plusieurs instances optimizer
└── DLQ RabbitMQ         → ne plus perdre de jobs

PHASE 6 — Chaos et sécurité
├── TLS + autocert       → HTTPS en prod
├── validation uploads   → sécurité des entrées
├── Pumba / Toxiproxy    → tester les pannes
└── JWT                  → authentification

PHASE 7 — Internals (comprendre en profondeur)
├── GC mark-and-sweep    → tuning mémoire avancé
├── escape analysis      → optimiser les allocations
├── epoll / io_uring     → I/O asynchrone niveau OS
└── gRPC + Protobuf      → remplacer HTTP/JSON inter-services
```

---

## Récapitulatif par difficulté

| Difficulté | Sujet | Temps estimé |
|---|---|---|
| ⭐ | wrk / vegeta | 1h |
| ⭐⭐ | pprof, rate limiting, graceful shutdown, ETags, SSE, structured logging | 1-2 jours |
| ⭐⭐⭐ | Prometheus, circuit breaker, context, cache L1, scaling horizontal, k6, DLQ, WebP | 1 semaine |
| ⭐⭐⭐⭐ | OpenTelemetry, GC tuning, escape analysis, consistent hashing, bloom filter, AVIF | 2-3 semaines |
| ⭐⭐⭐⭐⭐ | gRPC, HTTP/3, event sourcing, io_uring, epoll, zero-copy | 1-2 mois |
