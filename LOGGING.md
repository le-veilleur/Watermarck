# Cours : Structured Logging
## Des logs exploitables en production

---

## 📋 Table des matières

1. [Le problème des logs texte](#probleme)
2. [C'est quoi le structured logging ?](#intro)
3. [Les niveaux de log](#niveaux)
4. [zerolog — le plus rapide en Go](#zerolog)
5. [zap — l'alternative de Uber](#zap)
6. [Les champs à toujours inclure](#champs)
7. [Log sampling — ne pas tout logger](#sampling)
8. [Corrélation avec les traces (trace ID)](#correlation)
9. [Les backends — où envoyer les logs](#backends)
10. [Utilisation dans NWS Watermark](#watermark)
11. [Les anti-patterns à éviter](#antipatterns)
12. [Résumé](#résumé)

---

<a name="probleme"></a>
## 1. Le problème des logs texte

### Ce qu'on a actuellement

```
[API] ════════════════════════════════════════
[API] → Nouvelle requête reçue
[API] ① Lecture    : photo.jpg | 2.3 MB | 1.2ms
[API] ② SHA256     : a3f8c2d1e4b5... | calculé en 0.8ms
[API] ③ Redis      : ❌ CACHE MISS | lookup en 1.1ms
[API] ④ MinIO.Put  : ✓ Original sauvegardé | 2.3 MB | en 45ms
[API] ⑤ Optimizer  : ✓ 245.3 KB reçus en 312ms
[API] ⑥ Redis.Set  : ✓ 245.3 KB stockés | TTL 24h | en 0.9ms
[API] ⑦ Réponse    : gzip=true | taille=245.3 KB
[API] ⏱ Total      : 361ms
```

Ce log est **lisible par un humain** mais **inutilisable par une machine**.

**Problèmes concrets en production :**

```bash
# Trouver toutes les requêtes > 500ms → impossible
grep "Total" api.log | ???

# Compter les CACHE MISS par heure → impossible
grep "CACHE MISS" api.log | ???

# Corréler une erreur avec un utilisateur spécifique → impossible
grep "erreur" api.log  # aucune info sur qui a fait la requête

# Alerter si le taux d'erreur dépasse 1% → impossible sans parser du texte
```

**Le vrai problème :** les logs texte nécessitent du parsing fragile (regex) pour en extraire des données. Si le format du message change d'un caractère → le parsing casse.

---

### Ce qu'on veut

```json
{"level":"info","time":"2026-02-24T10:23:41Z","service":"api","request_id":"abc123","step":"optimizer","filename":"photo.jpg","bytes_in":2411520,"bytes_out":251187,"duration_ms":312,"cache":"miss"}
```

Une ligne = un objet JSON. N'importe quelle ligne est **requêtable** directement.

```bash
# Toutes les requêtes > 500ms
cat api.log | jq 'select(.duration_ms > 500)'

# Taux de cache miss par heure
cat api.log | jq -s 'group_by(.time[:13]) | map({hour: .[0].time[:13], miss_rate: (map(select(.cache=="miss")) | length) / length})'

# Corréler avec un request_id
cat api.log | jq 'select(.request_id == "abc123")'
```

---

<a name="intro"></a>
## 2. C'est quoi le structured logging ?

**Structured logging** = logger des **paires clé-valeur typées** plutôt que des chaînes de texte.

```
Non structuré :  "Optimizer a reçu 245 KB en 312ms pour photo.jpg"
Structuré :      {step:"optimizer", bytes:245187, duration_ms:312, filename:"photo.jpg"}
```

**Analogie :** c'est la différence entre écrire une note "Marie a appelé à 14h pour annuler le RDV de jeudi" vs remplir un formulaire avec les champs `contact`, `heure`, `action`, `date_rdv`. Le formulaire est cherchable, filtrable, agrégeable.

### Les 3 propriétés d'un bon log

1. **Structured** — JSON ou autre format machine-readable
2. **Leveled** — DEBUG, INFO, WARN, ERROR, FATAL
3. **Contextual** — chaque log porte le contexte (request_id, user_id, service...)

---

<a name="niveaux"></a>
## 3. Les niveaux de log

| Niveau | Usage | En prod ? |
|---|---|---|
| `TRACE` | Détails très fins (chaque pixel traité) | Jamais |
| `DEBUG` | Informations de développement | Non (trop verbeux) |
| `INFO` | Événements normaux (requête reçue, cache hit) | Oui |
| `WARN` | Situation anormale mais récupérable (retry, dégradation) | Oui |
| `ERROR` | Erreur qui nécessite attention (échec optimizer, erreur Redis) | Oui |
| `FATAL` | Erreur non récupérable → le processus s'arrête | Oui |

### La règle du niveau en production

```
Dev :    DEBUG → tout voir
Staging: INFO  → comportement normal + anomalies
Prod :   INFO  → comportement normal + anomalies
         WARN  → anomalies récupérables
         ERROR → incidents

# Jamais DEBUG en prod → trop de volume → coûts de stockage, signal noyé dans le bruit
```

### Changer le niveau dynamiquement

Les bons loggers permettent de changer le niveau sans redémarrer :

```go
// zerolog : changer le niveau global à la volée
zerolog.SetGlobalLevel(zerolog.DebugLevel)  // activer debug temporairement
zerolog.SetGlobalLevel(zerolog.InfoLevel)   // revenir à la normale
```

---

<a name="zerolog"></a>
## 4. zerolog — le plus rapide en Go

**zerolog** (développé par Olivier Poitrey de Netflix) est le logger Go le plus rapide — il alloue zéro byte pour les logs désactivés grâce aux interfaces `io.Writer` et à l'encodage direct en JSON.

```bash
go get github.com/rs/zerolog
```

### Setup de base

```go
import (
    "os"
    "github.com/rs/zerolog"
    "github.com/rs/zerolog/log"
)

func main() {
    // JSON en production
    zerolog.TimeFieldFormat = zerolog.TimeFormatUnix  // timestamp Unix (plus compact)
    log.Logger = zerolog.New(os.Stdout).With().
        Timestamp().
        Str("service", "api").
        Logger()

    // Pretty print en développement
    if os.Getenv("ENV") == "dev" {
        log.Logger = log.Output(zerolog.ConsoleWriter{Out: os.Stderr})
    }
}
```

### Logger des événements

```go
// INFO
log.Info().
    Str("step", "optimizer").
    Str("filename", "photo.jpg").
    Int("bytes_in", 2411520).
    Int("bytes_out", 251187).
    Dur("duration", optimizerDur).
    Msg("optimizer response received")

// Output :
// {"level":"info","service":"api","step":"optimizer","filename":"photo.jpg",
//  "bytes_in":2411520,"bytes_out":251187,"duration_ms":312,"time":1708772621,
//  "message":"optimizer response received"}
```

```go
// ERROR avec erreur Go
log.Error().
    Err(err).                         // ajoute le champ "error" avec err.Error()
    Str("step", "redis").
    Str("key", cacheKey[:16]).
    Msg("redis get failed")

// WARN
log.Warn().
    Str("step", "minio").
    Int("attempt", 2).
    Msg("minio put slow, retrying")
```

### Logger avec contexte (sous-logger)

```go
// Créer un sous-logger avec des champs fixés pour toute la durée du handler
func handleUpload(w http.ResponseWriter, r *http.Request) {
    requestID := generateRequestID()

    // Tous les logs de ce handler auront request_id et method automatiquement
    logger := log.With().
        Str("request_id", requestID).
        Str("method", r.Method).
        Str("path", r.URL.Path).
        Logger()

    logger.Info().Msg("request received")

    // Plus loin dans le handler...
    logger.Info().
        Str("step", "redis").
        Bool("hit", true).
        Dur("duration", redisDur).
        Msg("cache lookup")
}
```

### Logger dans un context.Context

```go
// Attacher le logger au contexte pour le propager sans le passer en paramètre
ctx := logger.WithContext(r.Context())

// Récupérer depuis le contexte
log.Ctx(ctx).Info().Str("step", "minio").Msg("saving original")
```

### Performance zerolog

```
BenchmarkInfo/zerolog  :   89 ns/op   0 B/op   0 allocs/op  ← zéro allocation
BenchmarkInfo/zap      :  131 ns/op   0 B/op   0 allocs/op
BenchmarkInfo/logrus   : 1256 ns/op 1297 B/op  24 allocs/op  ← 14x plus lent
BenchmarkInfo/slog     :  250 ns/op  48 B/op    2 allocs/op
```

zerolog est 14x plus rapide que logrus et ~3x plus rapide que slog (standard library Go 1.21).

---

<a name="zap"></a>
## 5. zap — l'alternative de Uber

**zap** (développé par Uber) est l'autre grand logger Go. Légèrement moins rapide que zerolog mais API plus riche.

```go
import "go.uber.org/zap"

// Logger de production (JSON)
logger, _ := zap.NewProduction()
defer logger.Sync()

// Logger de développement (couleurs, lisible)
logger, _ = zap.NewDevelopment()

logger.Info("optimizer response",
    zap.String("step", "optimizer"),
    zap.Int("bytes", 251187),
    zap.Duration("duration", optimizerDur),
)

// SugaredLogger — API moins stricte mais légèrement plus lente
sugar := logger.Sugar()
sugar.Infow("optimizer response",
    "step", "optimizer",
    "bytes", 251187,
)
```

### zerolog vs zap vs slog

| | zerolog | zap | slog (stdlib) |
|---|---|---|---|
| Performances | ⚡ Meilleur | ⚡ Très bon | Bon |
| API | Fluent (chaîné) | Typée stricte | Standard |
| Intégration stdlib | Non | Non | Oui (Go 1.21+) |
| Maintenance | Actif | Actif | Core team Go |
| Recommandation | Nouveaux projets perf-critiques | Projets Uber/enterprise | Projets qui veulent la stdlib |

**Conseil :** utiliser `zerolog` pour les microservices Go haute perf, `slog` si on veut zéro dépendance externe.

---

<a name="champs"></a>
## 6. Les champs à toujours inclure

### Champs obligatoires sur chaque log

```go
{
    "level":      "info",                    // niveau
    "time":       "2026-02-24T10:23:41Z",    // timestamp ISO 8601
    "service":    "api",                     // quel microservice
    "request_id": "f47ac10b-58cc-4372",      // identifiant unique de la requête
    "message":    "cache lookup"             // description humaine
}
```

### Champs contextuels selon l'étape

```go
// Requête HTTP entrante
{
    "method":     "POST",
    "path":       "/upload",
    "remote_ip":  "192.168.1.42",
    "user_agent": "Mozilla/5.0..."
}

// Opération Redis
{
    "step":       "redis",
    "key":        "a3f8c2d1...",   // les 16 premiers chars suffisent
    "hit":        false,
    "duration_ms": 1.1
}

// Appel optimizer
{
    "step":       "optimizer",
    "filename":   "photo.jpg",
    "bytes_in":   2411520,
    "bytes_out":  251187,
    "duration_ms": 312,
    "wm_position": "bottom-right"
}

// Erreur
{
    "level":     "error",
    "step":      "minio",
    "error":     "connection refused",
    "attempt":   2,
    "will_retry": true
}
```

### Conventions de nommage

```
snake_case pour les clés :  bytes_out  ✅   bytesOut  ❌
Unités dans le nom       :  duration_ms ✅  duration  ❌ (quelle unité ?)
Booléens explicites      :  cache_hit  ✅   hit       ❌ (hit quoi ?)
```

---

<a name="sampling"></a>
## 7. Log sampling — ne pas tout logger

**Le problème :** sous forte charge (1000 req/sec), logger chaque requête = 1000 lignes/sec = 86 millions/jour = des Go de logs par jour.

**Le sampling** : ne logger qu'une requête sur N pour les logs fréquents et non critiques.

```go
// zerolog : logger 1 requête sur 100 au niveau DEBUG
sampled := log.Sample(&zerolog.BasicSampler{N: 100})
sampled.Debug().
    Str("step", "redis").
    Bool("hit", true).
    Msg("cache hit")

// Logger 1 fois par seconde maximum (burst sampler)
sampled := log.Sample(zerolog.LevelSampler{
    DebugSampler: &zerolog.BurstSampler{
        Burst:       5,
        Period:      time.Second,
        NextSampler: &zerolog.BasicSampler{N: 100},
    },
})
```

### Stratégie de sampling par niveau

```
FATAL  : 100% — toujours logger (rare et critique)
ERROR  : 100% — toujours logger
WARN   : 100% — toujours logger
INFO   :  10% — 1 sur 10 suffit pour voir les tendances
DEBUG  :   1% — 1 sur 100 pour le diagnostic ponctuel
TRACE  :   0% — désactivé en prod
```

---

<a name="correlation"></a>
## 8. Corrélation avec les traces (request ID)

**Le problème :** un upload passe par API → Optimizer → Redis → MinIO. Les logs de ces 4 services sont dans 4 fichiers différents. Comment reconstituer le parcours d'une requête ?

**Solution : request ID propagé dans tous les headers et tous les logs.**

```go
// API : générer un request ID
func handleUpload(w http.ResponseWriter, r *http.Request) {
    requestID := r.Header.Get("X-Request-ID")
    if requestID == "" {
        requestID = uuid.New().String()
    }
    w.Header().Set("X-Request-ID", requestID)

    logger := log.With().Str("request_id", requestID).Logger()
    ctx := logger.WithContext(r.Context())

    // Propager vers l'optimizer
    sendToOptimizerWithID(ctx, requestID, ...)
}

// Dans sendToOptimizer : ajouter le header
req, _ := http.NewRequestWithContext(ctx, "POST", url, body)
req.Header.Set("X-Request-ID", requestID)
```

**Résultat :** on peut filtrer tous les logs d'une seule requête à travers tous les services :

```bash
cat api.log optimizer.log | jq 'select(.request_id == "f47ac10b")' | sort_by(.time)

# Output : tous les logs de la requête dans l'ordre chronologique
{"service":"api",       "time":"...001", "step":"read",      "bytes":2411520}
{"service":"api",       "time":"...002", "step":"redis",     "hit":false}
{"service":"api",       "time":"...003", "step":"minio",     "action":"put"}
{"service":"optimizer", "time":"...004", "step":"decode",    "format":"jpeg"}
{"service":"optimizer", "time":"...005", "step":"resize",    "from":"4000x3000","to":"1920x1080"}
{"service":"optimizer", "time":"...006", "step":"watermark", "duration_ms":267}
{"service":"api",       "time":"...007", "step":"redis",     "action":"set"}
```

---

<a name="backends"></a>
## 9. Les backends — où envoyer les logs

### stdout → collecteur → stockage

Le pattern standard en prod :

```
Service Go → stdout (JSON)
    │
    ▼
Collecteur (Filebeat / Fluent Bit / Vector)
    │
    ├── Loki (stockage logs, intégré Grafana)
    ├── Elasticsearch + Kibana (ELK Stack)
    ├── Datadog
    └── CloudWatch (AWS)
```

**Pourquoi écrire sur stdout et non dans un fichier ?**
- Les conteneurs Docker/Kubernetes capturent stdout automatiquement
- Pas de gestion de rotation de fichiers
- Le collecteur s'occupe du reste

### Loki — logs pour Grafana

Si on utilise déjà Prometheus + Grafana (section Observabilité), Loki s'intègre naturellement :

```yaml
# docker-compose.yml
loki:
  image: grafana/loki:latest
  ports:
    - "3100:3100"

promtail:
  image: grafana/promtail:latest
  volumes:
    - /var/lib/docker/containers:/var/lib/docker/containers:ro
  # Collecte les logs Docker et les envoie à Loki
```

**Requête Loki (LogQL) :**
```
{service="api"} | json | duration_ms > 500
{service="api"} | json | level="error" | error =~ "minio.*"
```

---

<a name="watermark"></a>
## 10. Utilisation dans NWS Watermark

### Migration de log.Printf vers zerolog

**Avant :**
```go
log.Printf("[API] ⑤ Optimizer  : ✓ %s reçus en %v", formatBytes(len(result)), optimizerDur)
```

**Après :**
```go
log.Info().
    Str("step", "optimizer").
    Str("request_id", requestID).
    Int("bytes", len(result)).
    Dur("duration", optimizerDur).
    Bool("success", true).
    Msg("optimizer response")
```

### Setup recommandé pour le projet

```go
// api/logger.go
package main

import (
    "os"
    "github.com/rs/zerolog"
    "github.com/rs/zerolog/log"
)

func initLogger() {
    zerolog.TimeFieldFormat = time.RFC3339

    log.Logger = zerolog.New(os.Stdout).With().
        Timestamp().
        Str("service", "api").
        Str("version", os.Getenv("VERSION")).
        Logger()

    // Pretty print si ENV=dev
    if os.Getenv("ENV") == "dev" {
        log.Logger = log.Output(zerolog.ConsoleWriter{
            Out:        os.Stderr,
            TimeFormat: "15:04:05",
        })
    }

    level, err := zerolog.ParseLevel(os.Getenv("LOG_LEVEL"))
    if err != nil {
        level = zerolog.InfoLevel
    }
    zerolog.SetGlobalLevel(level)
}
```

### Middleware de logging HTTP

```go
func loggingMiddleware(next http.Handler) http.Handler {
    return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        start := time.Now()

        // Générer ou récupérer le request ID
        requestID := r.Header.Get("X-Request-ID")
        if requestID == "" {
            requestID = ulid.Make().String()  // ou uuid.New().String()
        }
        w.Header().Set("X-Request-ID", requestID)

        // Sous-logger avec contexte de la requête
        logger := log.With().
            Str("request_id", requestID).
            Str("method", r.Method).
            Str("path", r.URL.Path).
            Str("remote_ip", r.RemoteAddr).
            Logger()

        // Injecter dans le contexte
        ctx := logger.WithContext(r.Context())

        // Wrapper pour capturer le status code
        wrapped := &responseWriter{ResponseWriter: w, status: 200}
        next.ServeHTTP(wrapped, r.WithContext(ctx))

        logger.Info().
            Int("status", wrapped.status).
            Dur("duration", time.Since(start)).
            Msg("request completed")
    })
}

type responseWriter struct {
    http.ResponseWriter
    status int
}
func (rw *responseWriter) WriteHeader(status int) {
    rw.status = status
    rw.ResponseWriter.WriteHeader(status)
}
```

---

<a name="antipatterns"></a>
## 11. Les anti-patterns à éviter

### ❌ Logger des données sensibles

```go
// JAMAIS logger des mots de passe, tokens, données personnelles
log.Info().Str("password", password).Msg("user login")   // ❌
log.Info().Str("token", jwt).Msg("auth success")          // ❌
log.Info().Str("email", user.Email).Msg("user upload")    // ❌ RGPD

// ✅ Logger des identifiants anonymisés
log.Info().Str("user_id", user.ID).Msg("user upload")
```

### ❌ Interpolation de chaînes dans les messages

```go
// ❌ Inutilisable — l'info est dans la chaîne, pas dans des champs
log.Info().Msgf("optimizer took %dms for %s", ms, filename)

// ✅ Champs typés
log.Info().Int("duration_ms", ms).Str("filename", filename).Msg("optimizer done")
```

### ❌ Ignorer les erreurs de logging

```go
// La plupart des loggers peuvent échouer silencieusement si le writer est fermé
// → utiliser defer logger.Sync() (zap) ou s'assurer que os.Stdout est ouvert
```

### ❌ Logger dans une boucle serrée

```go
// ❌ Logger chaque pixel → des millions de logs/sec
for py := 0; py < height; py++ {
    for px := 0; px < width; px++ {
        log.Debug().Int("px", px).Int("py", py).Msg("processing pixel")  // ❌
    }
}

// ✅ Logger le résumé
log.Debug().Int("pixels", width*height).Dur("duration", d).Msg("pixels processed")
```

### ❌ Mélanger logs texte et structurés

```go
log.Printf("erreur Redis")        // ❌ texte
log.Error().Msg("erreur Redis")   // ✅ structuré

// Choisir un format et s'y tenir dans tout le projet
```

---

<a name="résumé"></a>
## 12. Résumé

### Pourquoi passer au structured logging

```
log.Printf → lisible par humain, inutilisable par machine
zerolog    → JSON typé, requêtable, zéro allocation, 14x plus rapide que logrus
```

### Les règles à retenir

1. **Toujours JSON en prod** — texte lisible uniquement en dev (`ConsoleWriter`)
2. **Champs typés** — jamais `Msgf("took %dms")`, toujours `Int("duration_ms", ms)`
3. **request_id partout** — propager dans tous les services pour corréler les logs
4. **Sampling en prod** — ne pas logger 100% des requêtes INFO, 1-10% suffit
5. **Pas de données sensibles** — jamais de password, token, email en clair
6. **Niveau INFO par défaut** — DEBUG seulement en dev ou diagnostic ponctuel

### Stack recommandée

```
zerolog (logging) → stdout → Promtail/Fluent Bit (collecte) → Loki (stockage) → Grafana (visualisation)
```

### Comparaison rapide

| | log.Printf (actuel) | zerolog | zap | slog |
|---|---|---|---|---|
| Format | Texte | JSON | JSON | JSON |
| Requêtable | Non | Oui | Oui | Oui |
| Perf | Moyen | ⚡ Meilleur | ⚡ Très bon | Bon |
| Dépendance | stdlib | 1 pkg | 1 pkg | stdlib |
| Niveaux | Non | Oui | Oui | Oui |
| Sampling | Non | Oui | Oui | Partiel |
