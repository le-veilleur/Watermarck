# Goroutines & concurrence

Le projet utilise cinq patterns de concurrence distincts, chacun répondant à un besoin précis.

---

## 1. Sémaphore — worker pool (`optimizer/main.go:44`)

```go
var sem = make(chan struct{}, runtime.NumCPU())
```

**Pourquoi** : chaque requête d'optimisation charge une image en mémoire (~10-50 MB décompressée). Sans limite, N requêtes simultanées = N×50 MB → OOM. Le canal bufferisé joue le rôle de jeton : on ne peut traiter que `NumCPU` images en parallèle.

```go
slotsUsed := len(sem) + 1
sem <- struct{}{}          // bloque si tous les slots sont pris
defer func() {
    <-sem
    logger.Info()...Msg("slot libéré")
}()
```

**Pattern** : acquire avant traitement, release via `defer` — garantit la libération même en cas de panic.

---

## 2. WaitGroup + goroutines lock-free — `sampleLuminance` (`optimizer/main.go:303`)

**Pourquoi** : calculer la luminance moyenne d'une zone de l'image pixel par pixel est CPU-bound. On découpe les lignes en `NumCPU` tranches traitées en parallèle.

**Lock-free** : chaque goroutine écrit dans `totals[i]` (son propre index), aucun mutex nécessaire.

```go
totals := make([]float64, numWorkers)   // un slot par goroutine
var wg sync.WaitGroup

for i := 0; i < numWorkers; i++ {
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
        totals[idx] = t   // pas de contention — index distinct par goroutine
    }(rowStart, rowEnd, i)
}
wg.Wait()
```

**Fallback séquentiel** (`line 328`) : si `rows < numWorkers`, le surcoût de spawn dépasse le gain → on reste séquentiel.

**Formule** : ITU-R BT.601 — pondère vert (58,7 %) > rouge (29,9 %) > bleu (11,4 %).

---

## 3. Goroutine + `io.Pipe` — streaming multipart (`api/main.go:479`)

**Pourquoi** : envoyer une image à l'optimizer sans la charger deux fois en mémoire. `io.Pipe` crée un tuyau synchrone : la goroutine écrit pendant que `httpClient.Post` lit de l'autre côté.

```go
pr, pw := io.Pipe()
mw := multipart.NewWriter(pw)

go func() {
    part, err := mw.CreateFormFile("image", filename)
    if err != nil {
        pw.CloseWithError(err)   // propage l'erreur au Post
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
```

**Invariant** : si la goroutine échoue, `pw.CloseWithError(err)` propage l'erreur au `Post` — pas de goroutine leak.

---

## 4. Goroutine background — retry worker (`api/main.go:63`)

**Pourquoi** : le retraitement des jobs échoués (RabbitMQ) ne doit pas bloquer le serveur HTTP. On lance un worker dédié au démarrage.

```go
// dans main()
go retryWorker()
```

```go
func retryWorker() {
    amqpChan.Qos(1, 0, false)   // prefetch = 1 : traite un job à la fois
    msgs, _ := amqpChan.Consume("watermark_retry", ...)
    for msg := range msgs {
        processRetryJob(msg, optimizerURL)
    }
}
```

**Backoff** :

| Étape en échec | Action | Délai |
|---|---|---|
| MinIO KO | Nack + requeue | 5 s |
| Optimizer KO | Nack + requeue | 10 s |
| JSON invalide (poison pill) | Ack | — (éliminé) |

**QoS 1** : garantit qu'un seul message est en cours de traitement — évite les pics mémoire si la queue grossit.

---

## 5. `sync.Pool` — réutilisation de buffers (`optimizer/main.go:46`)

**Pourquoi** : l'encodage JPEG/WebP alloue un `bytes.Buffer` par requête. Avec plusieurs requêtes/s, le GC passerait son temps à collecter ces objets de plusieurs MB. Le pool les réutilise.

```go
var bufPool = sync.Pool{
    New: func() any { return new(bytes.Buffer) },
}

buf := bufPool.Get().(*bytes.Buffer)
buf.Reset()              // vider sans réallouer
defer bufPool.Put(buf)
// ... encoder dans buf ...
```

**Piège à éviter** : toujours `Reset()` avant usage, ne jamais retourner un buffer dont on a gardé une référence au slice interne.

---

## Interactions entre patterns

```
requête HTTP
    │
    ▼
sem <- struct{}{}        ← bloque si NumCPU slots pris
    │
    ├── sampleLuminance  → WaitGroup (NumCPU goroutines, lock-free)
    ├── encode JPEG/WebP → bufPool
    │
    └── réponse HTTP
            │
            └── si optimizer KO → publish RabbitMQ
                    │
                    └── retryWorker (goroutine bg)
                            ├── fetchFromMinio
                            └── sendToOptimizer → io.Pipe
```

---

## Modèle de threading & parallélisme

### Goroutines ≠ threads OS

Go utilise un modèle **M:N** : M goroutines multiplexées sur N threads OS, gérés par le scheduler du runtime.

```
Goroutines (M)     OS Threads (N)     CPU Cores
   G1 ──┐
   G2 ──┤──► Thread 1 ──► Core 0
   G3 ──┘
   G4 ──┐
   G5 ──┤──► Thread 2 ──► Core 1
   G6 ──┘
        ▲
    scheduler Go (runtime)
```

### GOMAXPROCS — vrai parallélisme

`GOMAXPROCS` contrôle combien de threads OS exécutent du code Go **simultanément** :

```go
runtime.GOMAXPROCS(0)              // retourne la valeur courante
runtime.GOMAXPROCS(runtime.NumCPU()) // défaut depuis Go 1.5
```

```bash
GOMAXPROCS=4 ./optimizer   # forcer via env
```

Défaut = `NumCPU()` → **vrai multi-threading activé par défaut**. Il n'y a rien à "forcer".

| GOMAXPROCS | Effet |
|---|---|
| 1 | Séquentiel — une goroutine à la fois |
| NumCPU() | Parallélisme maximal (défaut) |
| > NumCPU() | Rarement utile, surcoût de context-switch |

### Thread pinning

Go ne supporte **pas** le CPU affinity nativement. Il y a deux niveaux :

**1. `runtime.LockOSThread()` — goroutine collée à un thread OS (natif Go)**

```go
runtime.LockOSThread()
defer runtime.UnlockOSThread()
// cette goroutine restera sur le même thread OS
```

Ne choisit pas le cœur — garantit seulement que la goroutine ne migre pas vers un autre thread. Utile pour CGo avec état thread-local (OpenGL, libs C), ou syscalls spécifiques à un thread (Linux namespaces).

**2. CPU affinity — thread collé à un cœur (syscall Linux, non natif)**

```go
import "golang.org/x/sys/unix"

runtime.LockOSThread()   // obligatoire avant — sinon le scheduler peut migrer
var mask unix.CPUSet
mask.Set(0)              // cœur 0
unix.SchedSetaffinity(0, &mask)
```

Nécessite `LockOSThread()` d'abord, sinon la goroutine peut être déplacée vers un autre thread (et donc un autre cœur) par le scheduler Go.

**En pratique** : quasi jamais nécessaire. Le scheduler Go + l'OS placent les threads efficacement. Réservé aux systèmes temps-réel ou benchmarks ultra-précis.

---

## `sync/atomic` — opérations atomiques

### Principe

Une opération atomique est **indivisible** : aucun autre thread ne peut lire une valeur à moitié écrite. Sans atomicité, un compteur partagé incrémenté par plusieurs goroutines peut perdre des mises à jour (race condition).

```go
// Dangereux — race condition si plusieurs goroutines l'exécutent
counter++

// Sûr — lecture-modification-écriture atomique
atomic.AddInt64(&counter, 1)
```

### Types disponibles (Go 1.19+)

Depuis Go 1.19, le package expose des types génériques plus ergonomiques que les fonctions bas niveau :

```go
var counter atomic.Int64    // zéro-valeur prête à l'emploi, pas besoin d'init

counter.Add(1)
counter.Load()              // lire
counter.Store(42)           // écrire
counter.Swap(10)            // écrire et retourner l'ancienne valeur
counter.CompareAndSwap(10, 20)  // CAS : écrit 20 seulement si la valeur est 10
```

Types disponibles : `atomic.Int32`, `atomic.Int64`, `atomic.Uint32`, `atomic.Uint64`, `atomic.Uintptr`, `atomic.Bool`, `atomic.Pointer[T]`.

### Atomic vs Mutex

| | `sync/atomic` | `sync.Mutex` |
|---|---|---|
| Cas d'usage | Un seul scalaire (compteur, flag) | Struct complexe, section critique multi-champs |
| Performance | Très rapide (instruction CPU) | Plus lent (appel système possible) |
| Lisibilité | Moins expressive | Plus explicite |
| Composabilité | Non — chaque op est atomique séparément | Oui — groupe plusieurs ops |

**Règle** : si tu protèges un seul entier ou un seul booléen, utilise `atomic`. Dès que tu touches plusieurs champs ensemble, utilise `Mutex`.

### Cas d'usage concrets dans ce projet

Le projet n'utilise pas `sync/atomic` aujourd'hui, mais voici où ça aurait du sens :

```go
// Compteur de requêtes en cours (monitoring)
var activeRequests atomic.Int64

func handler(w http.ResponseWriter, r *http.Request) {
    activeRequests.Add(1)
    defer activeRequests.Add(-1)
    // ...
}

// Flag pour circuit-breaker optimizer
var optimizerDown atomic.Bool

if optimizerDown.Load() {
    // répondre directement depuis le cache ou erreur 503
}
```

### `atomic.Pointer[T]` — hot reload de config

Permet de remplacer atomiquement un pointeur de configuration sans mutex, pattern "publish-subscribe" :

```go
var cfg atomic.Pointer[Config]
cfg.Store(&Config{Quality: 85})

// goroutine de reload (signal SIGHUP par ex.)
go func() {
    cfg.Store(&Config{Quality: 90})   // remplacement instantané et sûr
}()

// chaque handler lit la config courante
current := cfg.Load()
```

### Ce que `atomic` ne remplace pas

- **Pas de transaction** : deux `atomic.Load` successifs ne sont pas atomiques entre eux.
- **Pas d'ordre garanti** sans `atomic.Value` ou memory barriers explicites dans les cas complexes.
- **Ne protège pas les slices/maps** — pour ça, `sync.RWMutex` ou `sync.Map`.

---

## `sync.Map` — map concurrente

### Pourquoi pas une `map` normale ?

Une `map` Go n'est pas thread-safe. Deux goroutines qui écrivent dessus simultanément provoquent un **fatal error** au runtime (détecté par le race detector) :

```go
// Dangereux
var cache map[string][]byte
go func() { cache["a"] = data }()   // race condition → crash
go func() { cache["b"] = data }()
```

### API

```go
var m sync.Map

m.Store("clé", valeur)              // écrire

v, ok := m.Load("clé")             // lire
if ok {
    data := v.([]byte)              // type assertion nécessaire
}

v, loaded := m.LoadOrStore("clé", valeur)  // écrire seulement si absent
                                            // retourne la valeur existante si présent

m.Delete("clé")                     // supprimer

m.Range(func(k, v any) bool {       // itérer (pas d'ordre garanti)
    fmt.Println(k, v)
    return true                     // false = arrêter l'itération
})
```

### `sync.Map` vs `map` + `sync.RWMutex`

| | `sync.Map` | `map` + `RWMutex` |
|---|---|---|
| Lectures fréquentes, écritures rares | Très rapide (lock-free en lecture) | Bien |
| Écritures fréquentes | Moins efficace | Mieux |
| Clés stables dans le temps | Optimisé pour ça | Neutre |
| Type-safety | Non (`any`) | Oui |
| Lisibilité | Verbose | Plus naturelle |

**Règle** : `sync.Map` est optimisée pour le cas "beaucoup de lectures, peu d'écritures, clés qui changent rarement" — typiquement un cache. Pour une map avec beaucoup d'écritures, `RWMutex` est plus performant.

### Cas d'usage concret dans ce projet

Le cache Redis pourrait être doublé d'un cache in-process pour les images très fréquentes :

```go
var localCache sync.Map   // clé = hash, valeur = []byte (image watermarkée)

// Lire depuis le cache local avant Redis
if v, ok := localCache.Load(cacheKey); ok {
    w.Write(v.([]byte))
    return
}

// Stocker après traitement
localCache.Store(cacheKey, result)
```

### Limites

- **Pas de TTL natif** : les entrées restent indéfiniment. Il faut gérer l'expiration manuellement (goroutine de nettoyage + `time.Time` dans la valeur).
- **Pas de taille maximale** : risque de fuite mémoire si les clés sont dynamiques et nombreuses.
- **Type assertion obligatoire** : la valeur est `any`, un mauvais cast panique au runtime.

---

## Règles à respecter

- Ne pas augmenter `cap(sem)` au-delà de `NumCPU` sans profiler la mémoire.
- Ne pas supprimer le fallback séquentiel dans `sampleLuminance` — le seuil `rows < numWorkers` est intentionnel.
- Toujours fermer le `PipeWriter` dans la goroutine (y compris en cas d'erreur via `CloseWithError`).
- Ne pas introduire de `sync.Mutex` dans `sampleLuminance` — le design lock-free est volontaire.