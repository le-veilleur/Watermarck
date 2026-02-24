# Cours : Résilience
## Ne pas tomber sous charge — Circuit Breaker, Rate Limiting, Retry, Context, Health Checks

---

## 📋 Table des matières

1. [C'est quoi la résilience ?](#intro)
2. [Circuit Breaker — fail-fast](#circuit-breaker)
3. [Rate Limiting — se protéger des abus](#rate-limiting)
4. [Retry avec backoff exponentiel + jitter](#retry)
5. [context.Context — annuler le travail inutile](#context)
6. [Health Checks — readiness et liveness](#health)
7. [Graceful Shutdown — zéro coupure](#shutdown)
8. [Bulkhead — isoler les défaillances](#bulkhead)
9. [Timeout — ne jamais attendre indéfiniment](#timeout)
10. [Utilisation dans NWS Watermark](#watermark)
11. [Résumé — les patterns combinés](#résumé)

---

<a name="intro"></a>
## 1. C'est quoi la résilience ?

**Résilience** = la capacité d'un système à **dégrader gracieusement** plutôt que s'effondrer brutalement quand quelque chose se passe mal.

**Analogie :** un disjoncteur électrique. Quand il y a un court-circuit, il coupe le courant plutôt que de laisser brûler toute la maison. Il ne répare pas la panne, mais il l'isole.

```
Système fragile (cascade failure) :
  Optimizer lent → API attend → toutes les goroutines bloquent → RAM épuisée → API crashe → front en erreur

Système résilient (degradation gracieuse) :
  Optimizer lent → circuit breaker s'ouvre → RabbitMQ prend le relais → API répond 202 → front poll
```

### Les 8 fallacies des systèmes distribués

Ces 8 hypothèses que les développeurs font à tort en débutant :

1. Le réseau est fiable
2. La latence est nulle
3. La bande passante est infinie
4. Le réseau est sécurisé
5. La topologie ne change pas
6. Il y a un seul administrateur
7. Le coût de transport est nul
8. Le réseau est homogène

**Un système résilient accepte que ces hypothèses soient fausses.**

---

<a name="circuit-breaker"></a>
## 2. Circuit Breaker — fail-fast

### Le problème sans circuit breaker

```
100 goroutines envoient des requêtes à l'optimizer
L'optimizer est KO → timeout à 30s

Sans circuit breaker :
  → 100 goroutines bloquées 30s chacune
  → 100 × 30s = 3000s de goroutines bloquées
  → chaque goroutine consomme ~8 KB de stack
  → 100 × 8 KB = 800 KB minimum (+ les buffers)
  → si les requêtes continuent d'arriver → mémoire épuisée → API crashe
```

### Les 3 états

```
          5 échecs consécutifs
CLOSED ────────────────────────► OPEN
(normal)                        (court-circuit)
   ▲                                │
   │    succès                      │ après 30 secondes
   │◄──────────── HALF-OPEN ◄───────┘
                 (1 requête test)
                      │
                      └─► échec → retour OPEN
```

**CLOSED (fermé) :** le circuit laisse passer les requêtes. On compte les échecs.

**OPEN (ouvert) :** le circuit bloque toutes les requêtes immédiatement sans même essayer. On retourne une erreur ou on bascule sur un fallback.

**HALF-OPEN (semi-ouvert) :** après le timeout, on laisse passer une seule requête test. Si elle réussit → CLOSED. Si elle échoue → OPEN.

### Implémentation avec gobreaker

```go
import "github.com/sony/gobreaker"

var optimizerCB *gobreaker.CircuitBreaker

func initCircuitBreaker() {
    optimizerCB = gobreaker.NewCircuitBreaker(gobreaker.Settings{
        Name:        "optimizer",
        MaxRequests: 1,              // 1 requête test en half-open
        Interval:    10 * time.Second, // réinitialise les compteurs toutes les 10s en CLOSED
        Timeout:     30 * time.Second, // temps avant de passer en HALF-OPEN depuis OPEN

        // Conditions pour passer en OPEN
        ReadyToTrip: func(counts gobreaker.Counts) bool {
            // Ouvrir si > 5 échecs consécutifs ET > 60% de taux d'échec
            failureRatio := float64(counts.TotalFailures) / float64(counts.Requests)
            return counts.ConsecutiveFailures >= 5 || (counts.Requests >= 10 && failureRatio >= 0.6)
        },

        // Callback sur changement d'état
        OnStateChange: func(name string, from, to gobreaker.State) {
            log.Warn().
                Str("breaker", name).
                Str("from", from.String()).
                Str("to", to.String()).
                Msg("circuit breaker state changed")
        },
    })
}

func sendToOptimizerWithBreaker(url, filename string, data []byte, wmText, wmPosition string) ([]byte, error) {
    result, err := optimizerCB.Execute(func() (interface{}, error) {
        return sendToOptimizer(url, filename, data, wmText, wmPosition)
    })

    if err == gobreaker.ErrOpenState {
        // Circuit ouvert → fallback immédiat sans attendre le timeout
        return nil, fmt.Errorf("optimizer circuit open, using fallback")
    }
    if err != nil {
        return nil, err
    }
    return result.([]byte), nil
}
```

### Intégration dans handleUpload

```go
tOptimizer := time.Now()
result, err := sendToOptimizerWithBreaker(optimizerURL, header.Filename, data, wmText, wmPosition)
if err != nil {
    // Que ce soit un vrai échec ou le circuit ouvert → même fallback RabbitMQ
    log.Warn().Err(err).Msg("optimizer unavailable, queuing job")
    replyWithRetryJob(w, ctx, cacheKey, originalKey, header.Filename, wmText, wmPosition, start)
    return
}
```

### Ce qu'on évite avec le circuit breaker

```
Sans CB : 100 req × 30s timeout = 3000s de goroutines bloquées
Avec CB : 5 req échouent → CB ouvert → les 95 suivantes : 0ms de blocage, fallback immédiat
```

---

<a name="rate-limiting"></a>
## 3. Rate Limiting — se protéger des abus

### Pourquoi limiter les requêtes ?

```
Sans rate limiting :
  Un client malveillant envoie 10 000 images/sec
  → l'optimizer est saturé pour tout le monde
  → les autres clients reçoivent des timeouts

Avec rate limiting :
  Chaque IP limitée à 10 req/sec
  → le client malveillant reçoit des 429 Too Many Requests
  → les autres clients ne voient rien
```

### Token Bucket — l'algorithme

```
Bucket capacity = 10 tokens
Refill rate     = 2 tokens/sec

T=0s  : bucket = [■■■■■■■■■■] 10 tokens
  3 requêtes simultanées :
T=0s  : bucket = [■■■■■■■░░░] 7 tokens  (-3)
T=1s  : bucket = [■■■■■■■■■░] 9 tokens  (+2, cap 10)
T=1s  : 5 requêtes → bucket = [■■■■░░░░░░] 4 tokens  (-5)
T=2s  : bucket = [■■■■■■░░░░] 6 tokens  (+2)
...

→ Bursts autorisés (jusqu'à 10 req instantanées)
→ Débit moyen limité à 2 req/sec sur le long terme
```

### Implémentation avec golang.org/x/time/rate

```go
import (
    "net/http"
    "sync"
    "time"
    "golang.org/x/time/rate"
)

// Un limiteur par IP
type ipLimiter struct {
    limiter  *rate.Limiter
    lastSeen time.Time
}

var (
    mu       sync.Mutex
    limiters = make(map[string]*ipLimiter)
)

func getLimiter(ip string) *rate.Limiter {
    mu.Lock()
    defer mu.Unlock()

    if l, ok := limiters[ip]; ok {
        l.lastSeen = time.Now()
        return l.limiter
    }

    // 10 req/sec, burst de 20
    l := &ipLimiter{
        limiter:  rate.NewLimiter(rate.Limit(10), 20),
        lastSeen: time.Now(),
    }
    limiters[ip] = l
    return l.limiter
}

// Nettoyer les entrées inactives depuis plus de 3 minutes
func cleanupLimiters() {
    for {
        time.Sleep(3 * time.Minute)
        mu.Lock()
        for ip, l := range limiters {
            if time.Since(l.lastSeen) > 3*time.Minute {
                delete(limiters, ip)
            }
        }
        mu.Unlock()
    }
}

// Middleware
func rateLimitMiddleware(next http.Handler) http.Handler {
    go cleanupLimiters()  // démarrer le nettoyage en arrière-plan

    return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        ip := r.RemoteAddr
        limiter := getLimiter(ip)

        if !limiter.Allow() {
            // Indiquer au client combien de temps attendre
            w.Header().Set("Retry-After", "1")
            w.Header().Set("X-RateLimit-Limit", "10")
            http.Error(w, "Too Many Requests", http.StatusTooManyRequests)
            return
        }

        next.ServeHTTP(w, r)
    })
}
```

### Sliding Window Counter — l'alternative plus précise

Token Bucket autorise des bursts. Sliding Window interdit les bursts :

```
Max 100 requêtes sur les 60 dernières secondes

T=0   : 100 requêtes arrivent → toutes acceptées (bucket = 100)
T=1s  : 1 requête → refusée (on est encore dans la fenêtre)
T=60s : la fenêtre glisse → les premières requêtes sortent de la fenêtre
T=60s : 100 nouvelles requêtes → acceptées

Implémentation Redis :
  ZADD ratelimit:{ip} {timestamp} {request_id}
  ZREMRANGEBYSCORE ratelimit:{ip} 0 {now - 60s}
  count = ZCARD ratelimit:{ip}
  if count > 100 → refuser
  EXPIRE ratelimit:{ip} 60
```

---

<a name="retry"></a>
## 4. Retry avec backoff exponentiel + jitter

### Le problème du retry fixe

```go
// ❌ Actuel dans processRetryJob
time.Sleep(10 * time.Second)
```

**Thundering Herd problem :**
```
T=0   : 1000 jobs échouent
T=10s : 1000 jobs réessaient SIMULTANÉMENT → pic de charge → ils rééchouent tous
T=20s : 1000 jobs réessaient SIMULTANÉMENT → pic de charge → ...
→ boucle infinie qui surcharge le service exactement quand il essaie de récupérer
```

### Backoff exponentiel

```go
// Chaque tentative double le délai d'attente
// Attempt 0 : 1s
// Attempt 1 : 2s
// Attempt 2 : 4s
// Attempt 3 : 8s
// Attempt 4 : 16s
// ...jusqu'au max de 5 minutes

func exponentialBackoff(attempt int) time.Duration {
    base := time.Second
    max  := 5 * time.Minute
    exp  := base << attempt  // base * 2^attempt
    if exp > max {
        exp = max
    }
    return exp
}
```

### Jitter — disperser les retries dans le temps

Sans jitter, tous les jobs qui échouent ensemble réessaient ensemble → même pic de charge.
Le jitter ajoute une durée aléatoire pour disperser les retries.

```go
import "math/rand"

// Full jitter : délai aléatoire entre 0 et exp
func fullJitter(attempt int) time.Duration {
    exp := exponentialBackoff(attempt)
    return time.Duration(rand.Int63n(int64(exp)))
}

// Equal jitter : exp/2 + random(exp/2) → garantit un minimum d'attente
func equalJitter(attempt int) time.Duration {
    exp := exponentialBackoff(attempt)
    half := exp / 2
    return half + time.Duration(rand.Int63n(int64(half)))
}
```

**Résultat avec equal jitter :**
```
1000 jobs échouent simultanément
  → Job 1   attend 2.3s
  → Job 2   attend 1.8s
  → Job 3   attend 3.1s
  → ...
  → les retries sont dispersés sur ~2s au lieu d'être simultanés
  → charge distribuée uniformément → le service récupère tranquillement
```

### Implémentation dans processRetryJob

```go
func processRetryJob(msg amqp.Delivery, optimizerURL string) {
    var job RetryJob
    if err := json.Unmarshal(msg.Body, &job); err != nil {
        msg.Ack(false)
        return
    }

    // Récupérer le numéro de tentative depuis les headers AMQP
    attempt := 0
    if headers := msg.Headers; headers != nil {
        if v, ok := headers["x-attempt"].(int32); ok {
            attempt = int(v)
        }
    }

    data, err := fetchFromMinio(job.OriginalKey)
    if err != nil {
        wait := equalJitter(attempt)
        log.Warn().Int("attempt", attempt).Dur("retry_in", wait).Msg("minio fetch failed, retrying")
        msg.Nack(false, true)
        time.Sleep(wait)
        return
    }

    result, err := sendToOptimizer(optimizerURL, job.Filename, data, job.WmText, job.WmPosition)
    if err != nil {
        if attempt >= 5 {
            // Trop de tentatives → envoyer en DLQ
            log.Error().Int("attempt", attempt).Msg("max retries exceeded, sending to DLQ")
            msg.Nack(false, false)  // false = ne pas requeue → ira en DLQ
            return
        }
        wait := equalJitter(attempt)
        log.Warn().Int("attempt", attempt).Dur("retry_in", wait).Msg("optimizer still KO")
        msg.Nack(false, true)
        time.Sleep(wait)
        return
    }

    redisClient.Set(context.Background(), job.Hash, result, 24*time.Hour)
    msg.Ack(false)
}
```

---

<a name="context"></a>
## 5. context.Context — annuler le travail inutile

### Le problème actuel

```
Client mobile upload une image → perd sa connexion WiFi à mi-chemin
→ API continue de traiter (MinIO, Optimizer, Redis)
→ 312ms de travail pour rien
→ Redis.Set stocke un résultat que personne ne lira jamais
→ en prod, des milliers de ces requêtes orphelines gaspillent des ressources
```

### context.Context — la solution Go

`context.Context` est une interface qui porte :
- Un **signal d'annulation** (la connexion client est coupée)
- Un **deadline** (maximum 10s pour répondre)
- Des **valeurs** (request_id, user_id propagés)

```go
// Hiérarchie de contextes
ctx := context.Background()          // racine, jamais annulée

// Annulation manuelle
ctx, cancel := context.WithCancel(ctx)
defer cancel()  // annule tout le sous-arbre à la fin

// Timeout global
ctx, cancel = context.WithTimeout(ctx, 10*time.Second)
defer cancel()

// Deadline absolue
ctx, cancel = context.WithDeadline(ctx, time.Now().Add(10*time.Second))
defer cancel()
```

### Propagation dans handleUpload

```go
func handleUpload(w http.ResponseWriter, r *http.Request) {
    // r.Context() est annulé automatiquement si le client déconnecte
    ctx := r.Context()

    // Timeout global pour toute la chaîne : 30 secondes max
    ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
    defer cancel()

    // ① Redis respecte le contexte — si client déconnecte, Redis.Get s'arrête
    data, err := redisClient.Get(ctx, cacheKey).Bytes()
    if errors.Is(err, context.Canceled) {
        log.Info().Msg("client disconnected during redis lookup")
        return
    }

    // ② MinIO respecte le contexte
    _, err = minioClient.PutObject(ctx, minioBucket, originalKey, ...)
    if err != nil {
        if errors.Is(err, context.DeadlineExceeded) {
            log.Warn().Msg("minio put timeout after 30s")
        }
        return
    }

    // ③ Timeout spécifique pour l'optimizer : 10s max
    optCtx, optCancel := context.WithTimeout(ctx, 10*time.Second)
    defer optCancel()

    result, err := sendToOptimizerWithContext(optCtx, optimizerURL, ...)
    if errors.Is(err, context.DeadlineExceeded) {
        log.Warn().Msg("optimizer timeout after 10s, queuing job")
        replyWithRetryJob(w, ctx, ...)
        return
    }
}
```

### Vérifier si le contexte est annulé dans une boucle

```go
// Pour les opérations longues, vérifier périodiquement
for _, chunk := range chunks {
    select {
    case <-ctx.Done():
        return ctx.Err()  // annulation ou timeout
    default:
    }
    processChunk(chunk)
}
```

### Propagation cross-service

```go
// Propager le contexte vers l'optimizer via HTTP
func sendToOptimizerWithContext(ctx context.Context, url, filename string, data []byte, ...) ([]byte, error) {
    req, err := http.NewRequestWithContext(ctx, "POST", url+"/optimize", pr)
    // Si ctx est annulé → la requête HTTP est annulée automatiquement
    resp, err := httpClient.Do(req)
    ...
}
```

---

<a name="health"></a>
## 6. Health Checks — readiness et liveness

### Les deux types de checks

**Liveness** : le processus est-il vivant ? (répondre OUI ou ne pas répondre)
→ Si liveness échoue → Kubernetes redémarre le pod

**Readiness** : le processus est-il prêt à recevoir du trafic ?
→ Si readiness échoue → Kubernetes retire le pod du load balancer (mais ne le redémarre pas)

```
Liveness  : "je tourne"           → vrai même si Redis est KO
Readiness : "je peux servir"      → faux si Redis ou MinIO est KO
```

### Implémentation

```go
// /health — liveness : juste "je suis vivant"
mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
    w.Header().Set("Content-Type", "application/json")
    json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
})

// /ready — readiness : toutes les dépendances sont-elles disponibles ?
mux.HandleFunc("GET /ready", func(w http.ResponseWriter, r *http.Request) {
    ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
    defer cancel()

    type check struct {
        name string
        err  error
    }
    checks := []check{
        {"redis", redisClient.Ping(ctx).Err()},
        {"minio", checkMinio(ctx)},
        {"rabbitmq", checkRabbitMQ()},
    }

    failed := map[string]string{}
    for _, c := range checks {
        if c.err != nil {
            failed[c.name] = c.err.Error()
        }
    }

    w.Header().Set("Content-Type", "application/json")
    if len(failed) > 0 {
        w.WriteHeader(http.StatusServiceUnavailable)
        json.NewEncoder(w).Encode(map[string]interface{}{
            "status": "not ready",
            "failed": failed,
        })
        return
    }

    json.NewEncoder(w).Encode(map[string]string{"status": "ready"})
})
```

### Configuration Docker Compose

```yaml
api:
  healthcheck:
    test: ["CMD", "wget", "-qO-", "http://localhost:3000/health"]
    interval: 10s
    timeout: 5s
    retries: 3
    start_period: 15s  # laisse le temps au service de démarrer
  depends_on:
    redis:
      condition: service_healthy
    minio:
      condition: service_healthy
    rabbitmq:
      condition: service_healthy
```

---

<a name="shutdown"></a>
## 7. Graceful Shutdown — zéro coupure

### Le problème d'un arrêt brutal

```
docker stop → SIGTERM → processus tué immédiatement

→ Requête en cours (optimize, 300ms) → perdue brutalement
→ Client reçoit une erreur réseau au lieu d'une réponse
→ En rolling deployment : quelques secondes de 50x errors
```

### Graceful shutdown

```go
func main() {
    srv := &http.Server{
        Addr:    ":3000",
        Handler: corsMiddleware(mux),

        // Timeouts pour éviter les connexions qui traînent
        ReadTimeout:  5 * time.Second,
        WriteTimeout: 60 * time.Second,
        IdleTimeout:  120 * time.Second,
    }

    // Démarrer le serveur dans une goroutine
    go func() {
        if err := srv.ListenAndServe(); err != http.ErrServerClosed {
            log.Fatal().Err(err).Msg("server error")
        }
    }()

    log.Info().Str("addr", ":3000").Msg("server started")

    // Attendre SIGTERM (docker stop) ou SIGINT (Ctrl+C)
    quit := make(chan os.Signal, 1)
    signal.Notify(quit, syscall.SIGTERM, syscall.SIGINT)
    sig := <-quit

    log.Info().Str("signal", sig.String()).Msg("shutdown initiated")

    // 30 secondes pour finir les requêtes en cours
    ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
    defer cancel()

    // Shutdown accepte les nouvelles connexions mais attend les existantes
    if err := srv.Shutdown(ctx); err != nil {
        log.Error().Err(err).Msg("forced shutdown after timeout")
    }

    log.Info().Msg("server stopped cleanly")
}
```

### Ordre d'arrêt propre

```
1. srv.Shutdown() → stop d'accepter de nouvelles connexions
2. Attendre que les handlers en cours finissent
3. Fermer la connexion RabbitMQ (après avoir NACKé les messages en cours)
4. Fermer Redis (flush les dernières commandes)
5. log.Info().Msg("bye")
6. os.Exit(0)
```

---

<a name="bulkhead"></a>
## 8. Bulkhead — isoler les défaillances

**Analogie :** les cloisons étanches d'un sous-marin. Si un compartiment est inondé, les autres restent secs.

**Sur NWS Watermark :** séparer les ressources (goroutines, connexions) pour que les uploads lents n'impactent pas les lookups rapides.

```go
// Worker pool séparé pour les uploads lents (optimizer)
var optimizerSem = make(chan struct{}, 10)  // max 10 uploads simultanés

// Worker pool séparé pour les lookups rapides (Redis)
var redisSem = make(chan struct{}, 50)      // max 50 lookups simultanés

// Si l'optimizer est saturé → les lookups (cache HIT) ne sont pas impactés
func handleUpload(w http.ResponseWriter, r *http.Request) {
    // Acquérir un slot optimizer (bloque si 10 uploads en cours)
    select {
    case optimizerSem <- struct{}{}:
        defer func() { <-optimizerSem }()
    case <-time.After(5 * time.Second):
        http.Error(w, "Service busy, try again", http.StatusServiceUnavailable)
        return
    }
    // ...
}
```

---

<a name="timeout"></a>
## 9. Timeout — ne jamais attendre indéfiniment

**Règle d'or :** chaque appel réseau doit avoir un timeout.

```go
// ✅ Timeouts sur le client HTTP
var httpClient = &http.Client{
    Timeout: 30 * time.Second,  // timeout global

    Transport: &http.Transport{
        DialContext: (&net.Dialer{
            Timeout:   3 * time.Second,   // timeout connexion TCP
            KeepAlive: 30 * time.Second,
        }).DialContext,
        TLSHandshakeTimeout:   5 * time.Second,
        ResponseHeaderTimeout: 10 * time.Second,  // timeout pour recevoir les headers
        IdleConnTimeout:       90 * time.Second,
        MaxIdleConns:          100,               // pool de connexions
        MaxIdleConnsPerHost:   10,
    },
}
```

### Timeout par opération

```
Opération            Timeout raisonnable
─────────────────────────────────────────
Connexion TCP        3s
TLS handshake        5s
Redis GET/SET        1s
MinIO PUT (2MB)      30s
Optimizer            15s
Réponse totale       60s
```

---

<a name="watermark"></a>
## 10. Utilisation dans NWS Watermark

### Ce qui manque actuellement

| Pattern | Statut | Impact si absent |
|---|---|---|
| Circuit Breaker | ❌ Manquant | Goroutines bloquées 30s si optimizer KO |
| Rate Limiting | ❌ Manquant | Abus possible, saturation optimizer |
| Backoff exponentiel | ❌ Manquant | Thundering herd sur les retries |
| context propagation | ❌ Manquant | Travail inutile si client déconnecte |
| Health checks | ❌ Manquant | Pas d'intégration Docker/Kubernetes |
| Graceful shutdown | ❌ Manquant | Requêtes perdues au déploiement |
| Bulkhead | ✅ Partiel | Worker pool côté optimizer uniquement |
| Timeouts HTTP | ✅ Partiel | `httpClient` a un timeout de 30s |

### Ordre d'implémentation recommandé

```
1. context + timeout    → le plus impactant, annule le travail inutile
2. Graceful shutdown    → zéro interruption au déploiement
3. Health checks        → intégration Docker Compose
4. Rate limiting        → se protéger des abus avant d'aller en prod
5. Circuit breaker      → si l'optimizer est un service externe critique
6. Backoff exponentiel  → améliorer les retries RabbitMQ
7. Bulkhead             → isoler uploads lents des lookups rapides
```

---

<a name="résumé"></a>
## 11. Résumé — les patterns combinés

### Vue d'ensemble

```
Requête entrante
      │
      ▼
Rate Limiter ──► 429 si abus
      │
      ▼
Context + Timeout (30s global)
      │
      ├──► Redis (timeout 1s)
      │     └── HIT → réponse directe
      │
      ├──► MinIO (timeout 30s)
      │
      ├──► Circuit Breaker ──► OPEN → fallback RabbitMQ immédiat
      │         │
      │         └── CLOSED → Optimizer (timeout 10s)
      │
      └──► context.Done() → annulation si client déconnecte
```

### Les 4 patterns essentiels

| Pattern | Protège contre | Implémentation Go |
|---|---|---|
| Circuit Breaker | Cascade failure | `sony/gobreaker` |
| Rate Limiting | Abus / surcharge | `golang.org/x/time/rate` |
| Context + Timeout | Travail inutile / attente infinie | `context.WithTimeout` |
| Graceful Shutdown | Perte de requêtes au déploiement | `http.Server.Shutdown()` |

### Règles à retenir

1. **Tout appel réseau a un timeout** — jamais d'attente infinie
2. **Fail-fast > fail-slow** — retourner une erreur rapidement vaut mieux que bloquer
3. **Dégradation gracieuse** — avoir toujours un fallback (RabbitMQ, 202, cache stale)
4. **Isoler les défaillances** — une panne ne doit pas cascader
5. **Mesurer d'abord** — ajouter le circuit breaker là où les pannes arrivent vraiment (pprof + Prometheus avant)
