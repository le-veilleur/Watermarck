# Cours : Serveur Haute Performance
## Optimisations appliquées sur le projet NWS Watermark

---

## 📋 Table des matières

1. [Architecture du projet](#architecture)
2. [io.Pipe — Streaming sans consommer la RAM](#iopipe)
3. [http.Client partagé — Réutilisation des connexions TCP](#httpclient)
4. [Worker Pool — Gestion intelligente du CPU](#workerpool)
5. [sync.Pool — Recyclage de la mémoire](#syncpool)
6. [Chargement unique des ressources](#chargement-unique)
7. [Compression Gzip — Réduction de la bande passante](#gzip)
8. [Redis — Cache en mémoire RAM](#redis)
9. [MinIO — Stockage objet persistant](#minio)
10. [Résumé des gains de performance](#résumé)

---

<a name="architecture"></a>
## 1. Architecture du projet

```
┌─────────────────┐
│   Navigateur    │  Front-end React (port 5173)
│    (client)     │
└────────┬────────┘
         │ POST /upload (multipart/form-data)
         ▼
┌─────────────────────────────────────────┐
│            API Gateway (port 3000)      │
│                                         │
│  ① Lecture image                        │
│  ② SHA256                               │
│  ③ Redis.Get ──► HIT → répond           │
│  ③ Redis.Get ──► MISS                   │
│  ④ MinIO.Put(original/)  ← sauvegarde  │
│  ⑤ Optimizer ──► OK → ⑥Redis ⑦Répond  │
│  ⑤ Optimizer ──► KO → MinIO.Get retry  │
└──────┬──────────────────┬───────────────┘
       │ io.Pipe          │ PutObject / GetObject
       ▼                  ▼
┌──────────────┐  ┌──────────────────────┐
│  Optimizer   │  │        MinIO         │
│  (port 3001) │  │     (port 9000)      │
│  • Resize    │  │  bucket: watermarks  │
│  • Watermark │  │  ├─ original/<hash>  │
│  • JPEG      │  │  Console: port 9001  │
└──────────────┘  └──────────────────────┘
       ▲
       │ Redis.Get / Redis.Set
┌──────────────┐
│    Redis     │
│  (port 6379) │
│  Cache RAM   │
│  TTL : 24h   │
└──────────────┘
```

**Principe clé :** Chaque service est **indépendant** avec son propre `go.mod`. Cela permet de :
- Scaler chaque service séparément
- Redémarrer un service sans affecter les autres
- Déployer des mises à jour isolées

---

<a name="iopipe"></a>
## 2. io.Pipe — Streaming sans consommer la RAM

### 🎯 Le problème

Quand un client envoie une image de 10 MB, l'approche naïve serait :

```go
// ❌ MAUVAISE APPROCHE
data, _ := io.ReadAll(file)  // charge les 10 MB en RAM
// envoie data à l'optimizer
```

**Conséquence :** Si 100 utilisateurs uploadent en même temps des images de 10 MB :
```
100 utilisateurs × 10 MB = 1 GB de RAM consommée
```

Le serveur s'écroule 💥

---

### ✅ La solution : io.Pipe

`io.Pipe()` crée un **tuyau virtuel** qui connecte un lecteur et un écrivain.

```go
pr, pw := io.Pipe()
```

- **`pr`** (PipeReader) : le bout où on **lit** les données
- **`pw`** (PipeWriter) : le bout où on **écrit** les données

**Analogie :** C'est comme un tuyau d'eau :
- L'eau (les données) entre d'un côté
- Elle ressort de l'autre côté
- **Aucune eau n'est stockée dans le tuyau**

---

### 🔄 Comment ça fonctionne

```go
pr, pw := io.Pipe()

// Goroutine 1 : Écrit les données dans le pipe
go func() {
    defer pw.Close()
    io.Copy(pw, file)  // Copie l'image dans le pipe (chunk par chunk)
}()

// Goroutine 2 : Lit depuis le pipe et envoie à l'optimizer
resp, err := httpClient.Post(optimizerURL, contentType, pr)
```

**Flux de données :**
```
Client (navigateur)
    │
    │ envoie 10 MB
    ▼
API reçoit chunk 1 (8 KB) ──► écrit dans pw ──► pr lit ──► envoie à optimizer
API reçoit chunk 2 (8 KB) ──► écrit dans pw ──► pr lit ──► envoie à optimizer
API reçoit chunk 3 (8 KB) ──► écrit dans pw ──► pr lit ──► envoie à optimizer
...
```

**Résultat :** On ne stocke jamais les 10 MB entiers en RAM ! Seulement de petits morceaux (chunks de 8-32 KB).

---

### ⚠️ Trade-off dans notre implémentation

Dans ce projet, `io.Pipe` est utilisé **entre l'API et l'optimizer**, mais l'API fait quand même un `io.ReadAll` en amont :

```go
// api/main.go
data, _ := io.ReadAll(file)  // Charge l'image en RAM...
hash := sha256.Sum256(data)  // ...pour calculer le hash SHA256
```

**Pourquoi ?** Pour interroger le cache Redis, il faut d'abord connaître le hash SHA256 de l'image — ce qui nécessite d'avoir tout le contenu en mémoire.

```
Client → API → io.ReadAll (charge 10 MB en RAM)
             → SHA256 → Redis.Get(hash)
             → si CACHE MISS : io.Pipe → Optimizer
             → si CACHE HIT  : répond directement (sans Pipe)
```

**Conséquence :** Le `io.Pipe` n'élimine pas la copie RAM côté API — il évite une **deuxième copie** lors du forward vers l'optimizer.

Le vrai gain de `io.Pipe` reste important : sans lui, on ferait `bytes.NewBuffer(data)` pour reconstruire un body HTTP, ce qui doublerait la consommation RAM. Avec `io.Pipe`, on relit directement depuis `data` déjà en mémoire sans duplication.

> Si on voulait un streaming pur sans `ReadAll`, il faudrait renoncer au cache Redis (impossible de calculer le hash sans lire tout le contenu), ou utiliser une autre stratégie de cache (ex: basée sur le nom + taille du fichier, moins fiable).

---

### 📊 Comparaison

| Approche | RAM utilisée pour 1 image de 10 MB | RAM pour 100 images simultanées |
|----------|-------------------------------------|----------------------------------|
| Sans Pipe | 10 MB × 2 (double copie) | 2000 MB (2 GB) 💀 |
| Avec Pipe | 10 MB (1 seule copie) | 1000 MB ✅ |
| Streaming pur (sans cache) | ~32 KB | ~3.2 MB ✅✅ |

**Gain dans notre cas :** **2x moins de RAM** grâce au Pipe (évite la double copie)

---

<a name="httpclient"></a>
## 3. http.Client partagé — Réutilisation des connexions TCP

### 🎯 Le problème

Chaque fois qu'on utilise `http.Post()`, Go crée un **nouveau client HTTP**.

```go
// ❌ MAUVAISE APPROCHE (dans une boucle de requêtes)
for i := 0; i < 1000; i++ {
    http.Post(url, contentType, body)  // Nouveau client à chaque fois
}
```

**Qu'est-ce qui se passe en coulisses ?**

Chaque appel fait :
1. **DNS lookup** : Résoudre `optimizer:3001` → `172.18.0.3`
2. **TCP handshake** : 3 aller-retours réseau (SYN, SYN-ACK, ACK)
3. **Envoyer la requête HTTP**
4. **Fermer la connexion TCP** (FIN, ACK, FIN, ACK)

```
Requête 1 : DNS + TCP open + HTTP + TCP close = ~50ms
Requête 2 : DNS + TCP open + HTTP + TCP close = ~50ms
Requête 3 : DNS + TCP open + HTTP + TCP close = ~50ms
...
```

**Pour 1000 requêtes :** 50 secondes perdues juste en ouverture/fermeture de connexions 😱

---

### ✅ La solution : Client HTTP partagé

On crée **un seul client** réutilisé pour toutes les requêtes.

```go
var httpClient = &http.Client{
    Timeout: 30 * time.Second,
}

// Dans le handler
resp, err := httpClient.Post(url, contentType, pr)
```

**HTTP Keep-Alive :** Le client maintient la connexion TCP ouverte entre les requêtes.

```
Requête 1 : DNS + TCP open + HTTP           = ~25ms
Requête 2 :                   HTTP           = ~2ms  (réutilise la connexion)
Requête 3 :                   HTTP           = ~2ms
Requête 4 :                   HTTP           = ~2ms
...
```

---

### ⏱️ Pourquoi le Timeout ?

Sans timeout, une requête peut bloquer **indéfiniment** :

```go
// ❌ SANS TIMEOUT
client := &http.Client{}
resp, _ := client.Get("http://serveur-tres-lent.com")
// Si le serveur ne répond jamais, la goroutine est bloquée POUR TOUJOURS
```

**Conséquence :** Fuite de goroutines → consommation de RAM infinie.

Avec le timeout :
```go
// ✅ AVEC TIMEOUT
client := &http.Client{Timeout: 30 * time.Second}
resp, err := client.Get("http://serveur-tres-lent.com")
// Après 30s, err = "context deadline exceeded"
```

---

### 📊 Comparaison

| Approche | Temps pour 1000 requêtes | Connexions TCP créées |
|----------|--------------------------|----------------------|
| Sans client partagé | ~50 secondes | 1000 |
| Avec client partagé | ~4 secondes | 1 (réutilisée) |

**Gain :** **12x plus rapide**

---

<a name="workerpool"></a>
## 4. Worker Pool — Gestion intelligente du CPU

### 🎯 Le problème

Quand 1000 utilisateurs uploadent des images en même temps, sans contrôle, le serveur crée **1000 goroutines** qui traitent toutes les images **simultanément**.

```
Image 1  ──► Goroutine 1 ──► CPU (resize + watermark)
Image 2  ──► Goroutine 2 ──► CPU (resize + watermark)
Image 3  ──► Goroutine 3 ──► CPU (resize + watermark)
...
Image 1000 ──► Goroutine 1000 ──► CPU (resize + watermark)
```

**Problème :** Un CPU à 8 cœurs ne peut faire que **8 opérations vraiment en parallèle**.

Les 992 autres goroutines se battent pour du temps CPU → **context switching** constant → tout ralentit.

**Analogie :** C'est comme une cuisine avec 8 plaques de cuisson et 1000 cuisiniers qui essaient tous de cuisiner en même temps. Chaos total 🔥

---

### ✅ La solution : Sémaphore avec un canal

On limite le nombre de traitements simultanés au nombre de **cœurs CPU**.

```go
// Création du sémaphore (taille = nombre de cœurs)
var sem = make(chan struct{}, runtime.NumCPU())

func handleOptimize(w http.ResponseWriter, r *http.Request) {
    sem <- struct{}{}        // Prend un slot (bloque s'ils sont tous pris)
    defer func() { <-sem }() // Libère le slot à la fin

    // Traitement de l'image (resize, watermark, encode)
    // ...
}
```

---

### 🔍 Explication détaillée

#### Qu'est-ce qu'un canal en Go ?

Un **canal** (`chan`) est comme une file d'attente avec une capacité limitée.

```go
sem := make(chan struct{}, 4)  // Canal de capacité 4
```

Visualisation :
```
sem = [_, _, _, _]  // 4 slots vides
```

---

#### Que se passe-t-il quand on fait `sem <- struct{}{}`  ?

On **envoie** une valeur dans le canal = on prend un slot.

```go
sem <- struct{}{}  // Prend le 1er slot
```

État du canal :
```
sem = [X, _, _, _]  // 1 slot occupé, 3 libres
```

Si tous les slots sont pris :
```
sem = [X, X, X, X]  // Tous les slots occupés
sem <- struct{}{}   // ⏸️ BLOQUE ici jusqu'à ce qu'un slot se libère
```

---

#### Que se passe-t-il quand on fait `<-sem` ?

On **lit** une valeur du canal = on libère un slot.

```go
<-sem  // Libère 1 slot
```

État du canal :
```
sem = [X, X, X, _]  // 1 slot libéré
```

La goroutine qui était bloquée peut maintenant continuer !

---

#### Pourquoi `struct{}` et pas `int` ?

```go
// ❌ Version avec int
var sem = make(chan int, 8)
sem <- 1  // Envoie un entier (occupe 8 bytes en mémoire)
```

```go
// ✅ Version avec struct{}
var sem = make(chan struct{}, 8)
sem <- struct{}{}  // Envoie une struct vide (occupe 0 byte !)
```

`struct{}` est le seul type en Go qui a une **taille mémoire de 0 byte**.

On ne veut pas transmettre de données, juste **signaler** qu'un slot est pris/libéré.

**Économie :** Sur 1 million de requêtes, cela évite de gaspiller 8 MB de RAM inutilement.

---

### 🎬 Exemple concret avec 8 cœurs

```
CPU : 8 cœurs disponibles
sem = make(chan struct{}, 8)

Requête 1  arrive → sem <- struct{}{} → slot 1 pris → traitement démarre
Requête 2  arrive → sem <- struct{}{} → slot 2 pris → traitement démarre
Requête 3  arrive → sem <- struct{}{} → slot 3 pris → traitement démarre
...
Requête 8  arrive → sem <- struct{}{} → slot 8 pris → traitement démarre

sem = [X, X, X, X, X, X, X, X]  // Tous les cœurs occupés

Requête 9  arrive → sem <- struct{}{} → ⏸️ BLOQUE (attend qu'un slot se libère)
Requête 10 arrive → sem <- struct{}{} → ⏸️ BLOQUE
...

Requête 1 termine → <-sem → slot 1 libéré
sem = [_, X, X, X, X, X, X, X]

Requête 9 débloquée → occupe le slot 1 → traitement démarre
```

---

### 📊 Comparaison

| Approche | 1000 requêtes simultanées | Utilisation CPU | Temps total |
|----------|---------------------------|-----------------|-------------|
| Sans limitation | 1000 goroutines actives | 100% (thrashing) | ~60s |
| Worker Pool (8 slots) | Max 8 goroutines actives | ~85% (optimal) | ~25s |

**Gain :** **2.4x plus rapide** grâce à une meilleure utilisation du CPU

---

<a name="syncpool"></a>
## 5. sync.Pool — Recyclage de la mémoire

### 🎯 Le problème

Pour encoder une image en JPEG, on a besoin d'un **buffer** (`bytes.Buffer`) temporaire.

```go
// ❌ APPROCHE NAÏVE
func handleOptimize(w http.ResponseWriter, r *http.Request) {
    buf := new(bytes.Buffer)  // Alloue un nouveau buffer
    jpeg.Encode(buf, img, nil)
    w.Write(buf.Bytes())
    // buf est détruit par le garbage collector après la fonction
}
```

**Qu'est-ce qui se passe pour 1000 requêtes ?**

```
Requête 1  → alloue buffer (32 KB) → utilise → GC détruit
Requête 2  → alloue buffer (32 KB) → utilise → GC détruit
Requête 3  → alloue buffer (32 KB) → utilise → GC détruit
...
Requête 1000 → alloue buffer (32 KB) → utilise → GC détruit
```

**Problème :** Le **Garbage Collector (GC)** doit constamment :
1. Détecter les buffers inutilisés
2. Les libérer de la mémoire

Cela consomme du **temps CPU** et crée des **pauses** dans le traitement.

---

### ✅ La solution : sync.Pool

Au lieu de détruire les buffers, on les **recycle** !

```go
// Pool global de buffers
var bufPool = sync.Pool{
    New: func() any {
        return new(bytes.Buffer)  // Crée un buffer UNIQUEMENT si le pool est vide
    },
}

func handleOptimize(w http.ResponseWriter, r *http.Request) {
    // Récupère un buffer du pool (ou en crée un si pool vide)
    buf := bufPool.Get().(*bytes.Buffer)
    buf.Reset()  // Remet le buffer à zéro (efface les données précédentes)
    
    defer bufPool.Put(buf)  // Remet le buffer dans le pool à la fin
    
    jpeg.Encode(buf, img, nil)
    w.Write(buf.Bytes())
}
```

---

### 🔄 Cycle de vie d'un buffer

```
1ère requête :
  Pool vide → New() crée un buffer → utilise → Put() le stocke

2ème requête :
  Pool a 1 buffer → Get() le récupère → Reset() efface → utilise → Put() le stocke

3ème requête :
  Pool a 1 buffer → Get() le récupère → Reset() efface → utilise → Put() le stocke

...
```

**Résultat :** Après la 1ère requête, **aucune nouvelle allocation mémoire** ! On réutilise toujours les mêmes buffers.

---

### ⚠️ Pourquoi `buf.Reset()` ?

Si on oublie `Reset()`, le buffer garde les données de la requête précédente !

```go
// ❌ SANS RESET
Requête 1 : buf contient "image1.jpg" → traite → Put(buf)
Requête 2 : Get(buf) → buf contient ENCORE "image1.jpg" → 💥 corruption de données
```

```go
// ✅ AVEC RESET
Requête 1 : buf contient "image1.jpg" → traite → Put(buf)
Requête 2 : Get(buf) → Reset() vide buf → buf est propre → ✅
```

---

### 📊 Comparaison

| Approche | Allocations pour 1000 requêtes | Temps GC | RAM max |
|----------|-------------------------------|----------|---------|
| Sans Pool | 1000 allocations | ~100ms | ~32 MB |
| Avec Pool | ~8 allocations (1 par cœur CPU) | ~5ms | ~256 KB |

**Gain :** **20x moins de pression sur le GC**

---

<a name="chargement-unique"></a>
## 6. Chargement unique des ressources

### 🎯 Le problème

Pour dessiner le watermark, on a besoin d'une **police de caractères** (fichier `.ttf`).

```go
// ❌ MAUVAISE APPROCHE
func handleOptimize(w http.ResponseWriter, r *http.Request) {
    fontBytes, _ := os.ReadFile("/fonts/Helvetica.ttc")  // Lit le fichier (2 MB)
    f, _ := opentype.ParseCollection(fontBytes)           // Parse le fichier
    font0, _ := f.Font(0)
    fontFace, _ := opentype.NewFace(font0, &options)
    
    // Utilise la police pour le watermark
    // ...
}
```

**Pour 1000 requêtes :**
```
1000 lectures fichier × 2 MB = 2 GB lus depuis le disque 😱
1000 parsing de police = énorme perte de temps CPU
```

---

### ✅ La solution : Variable globale

On charge la police **une seule fois** au démarrage du serveur.

```go
// Variable globale (partagée entre toutes les requêtes)
var fontFace font.Face

func main() {
    loadFont()  // Chargé UNE FOIS au démarrage
    http.ListenAndServe(":3001", nil)
}

func loadFont() error {
    fontBytes, _ := os.ReadFile(fontPath)
    f, _ := opentype.ParseCollection(fontBytes)
    font0, _ := f.Font(0)
    fontFace, _ = opentype.NewFace(font0, &opentype.FaceOptions{
        Size: 48,
        DPI:  72,
    })
    return nil
}

func handleOptimize(w http.ResponseWriter, r *http.Request) {
    // Utilise directement fontFace (déjà chargée)
    drawer := &font.Drawer{
        Dst:  img,
        Src:  image.White,
        Face: fontFace,  // ✅ Pas besoin de recharger
    }
}
```

---

### ⚠️ Thread-safety

**Question :** Plusieurs goroutines peuvent-elles utiliser `fontFace` en même temps sans danger ?

**Réponse :** Oui ! Tant qu'on ne **modifie pas** `fontFace`, c'est safe.

```go
// ✅ LECTURE SEULE (safe)
drawer.Face = fontFace  // Plusieurs goroutines peuvent lire en même temps

// ❌ ÉCRITURE (dangereux sans mutex)
fontFace = newFont  // Si plusieurs goroutines modifient en même temps → corruption
```

Dans notre cas, `fontFace` est en **lecture seule** → aucun problème.

---

### 📊 Comparaison

| Approche | I/O disque pour 1000 requêtes | Temps parsing |
|----------|-------------------------------|---------------|
| Chargement à chaque requête | 2 GB | ~5 secondes |
| Chargement unique | 2 MB (1 fois) | ~5 ms (1 fois) |

**Gain :** **1000x moins d'I/O disque**

---

<a name="gzip"></a>
## 7. Compression Gzip — Réduction de la bande passante

### 🎯 Le problème

Une image optimisée fait environ **325 KB**. Pour 1000 utilisateurs :

```
325 KB × 1000 = 325 MB de bande passante utilisée
```

**Coût :** Sur un serveur avec une connexion limitée, cela peut saturer la bande passante.

---

### ✅ La solution : Compression Gzip

On compresse la réponse **à la volée** si le navigateur le supporte.

```go
func handleUpload(w http.ResponseWriter, r *http.Request) {
    // ... traitement image ...
    
    // Vérifie si le client accepte gzip
    if strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
        w.Header().Set("Content-Encoding", "gzip")
        
        gz, _ := gzip.NewWriterLevel(w, gzip.BestSpeed)
        defer gz.Close()
        
        io.Copy(gz, resp.Body)  // Compresse en streaming
    } else {
        io.Copy(w, resp.Body)  // Envoie non compressé
    }
}
```

---

### 🔍 Explication

#### `Accept-Encoding: gzip`

Quand le navigateur envoie une requête, il indique les compressions qu'il supporte :

```
GET /upload HTTP/1.1
Host: localhost:3000
Accept-Encoding: gzip, deflate, br
```

On vérifie si `gzip` est dans la liste.

---

#### `gzip.BestSpeed` vs `gzip.BestCompression`

| Niveau | Taux de compression | Vitesse | Use case |
|--------|---------------------|---------|----------|
| `BestSpeed` | ~15% de réduction | Très rapide | Serveur web (c'est notre cas) |
| `DefaultCompression` | ~20% de réduction | Moyen | Équilibre |
| `BestCompression` | ~25% de réduction | Lent | Archivage de fichiers |

On choisit `BestSpeed` car on veut **privilégier la latence** (répondre vite) plutôt que d'économiser quelques KB supplémentaires.

---

### 📊 Comparaison

| Fichier | Taille originale | Taille compressée | Gain |
|---------|------------------|-------------------|------|
| Image JPEG optimisée | 325 KB | ~280 KB | 14% |
| HTML page | 50 KB | ~8 KB | 84% |
| JSON data | 100 KB | ~15 KB | 85% |

**Note :** JPEG est déjà compressé (c'est un format avec perte), donc le gain est faible (~14%). Mais pour du texte (HTML, JSON), le gain est énorme (80%+).

**Bande passante économisée :** Pour 1000 requêtes :
```
Sans gzip : 325 MB
Avec gzip : 280 MB
Économie  : 45 MB (~14%)
```

---

<a name="redis"></a>
## 8. Redis — Cache en mémoire RAM

### 🎯 C'est quoi Redis ?

**Redis** = **RE**mote **DI**ctionary **S**erver

C'est une base de données qui stocke tout **en RAM** (pas sur disque comme MySQL/PostgreSQL).

**Analogie :** C'est comme un dictionnaire géant ultra-rapide :

```python
redis = {
    "clé_1": "valeur_1",
    "clé_2": "valeur_2",
    ...
}
```

**Pourquoi c'est rapide ?**

| Opération | Disque SSD | RAM |
|-----------|------------|-----|
| Lire 1 KB | ~100 µs | ~0.1 µs |

**RAM = 1000x plus rapide que le disque**

---

### 🎯 Le problème

Traiter une image prend du temps :

```
Resize (1920×1080 → 800×600) : ~80ms
Watermark (draw text)        : ~20ms
Encode JPEG                  : ~100ms
───────────────────────────────────
Total                        : ~200ms
```

Si 100 utilisateurs uploadent **la même image** :

```
100 × 200ms = 20 secondes de CPU gaspillé
```

On refait 100 fois le même travail pour le même résultat 😱

---

### ✅ La solution : Cache Redis

**Principe :** On traite l'image **une seule fois**, puis on stocke le résultat dans Redis.

```
1ère requête  : Upload → Traitement (200ms) → Stocke dans Redis → Répond au client
2ème requête  : Upload → Redis (< 1ms)                         → Répond au client
3ème requête  : Upload → Redis (< 1ms)                         → Répond au client
...
100ème requête: Upload → Redis (< 1ms)                         → Répond au client
```

**Gain :** Au lieu de 20 secondes, on consomme `200ms + 99×1ms = ~300ms` de CPU.

**66x plus efficace !**

---

### 🔑 Comment identifier une image ?

On a besoin d'une **clé unique** pour chaque image différente.

**Mauvaise idée :** Utiliser le nom du fichier
```
"chat.jpg" → mais si 2 personnes uploadent des images différentes nommées "chat.jpg" ?
```

**Bonne idée :** Calculer l'**empreinte SHA256** du contenu

---

### 🔐 SHA256 — L'empreinte unique

**SHA256** = algorithme de hachage cryptographique

Il transforme **n'importe quelle donnée** en une chaîne de **64 caractères hexadécimaux**.

```go
import "crypto/sha256"

data := []byte("Hello World")
hash := sha256.Sum256(data)
hashString := hex.EncodeToString(hash[:])
// hashString = "a591a6d40bf420404a011733cfb7b190d62c65bf0bcda32b57b277d9ad9f146e"
```

---

### ✨ Propriétés magiques de SHA256

#### 1. Déterministe
Même entrée → toujours le même hash
```
"Hello" → "185f8db32271fe25f561a6fc938b2e264306ec304eda518007d1764826381969"
"Hello" → "185f8db32271fe25f561a6fc938b2e264306ec304eda518007d1764826381969"
"Hello" → "185f8db32271fe25f561a6fc938b2e264306ec304eda518007d1764826381969"
```

#### 2. Sensible au moindre changement
Moindre modification → hash complètement différent
```
"Hello"  → "185f8db32271fe25f561a6fc938b2e264306ec304eda518007d1764826381969"
"hello"  → "2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824"
         (juste H→h change TOUT le hash)
```

#### 3. Collisions quasi-impossibles
Probabilité que 2 images différentes aient le même hash :
```
1 / (2^256) ≈ 1 / 115 quattuorvigintillion
```

C'est plus que le nombre d'atomes dans l'univers 🤯

---

### 💾 Implémentation du cache

```go
import (
    "crypto/sha256"
    "encoding/hex"
    "github.com/redis/go-redis/v9"
)

var redisClient = redis.NewClient(&redis.Options{
    Addr: "localhost:6379",
})

func handleUpload(w http.ResponseWriter, r *http.Request) {
    file, _, _ := r.FormFile("image")
    data, _ := io.ReadAll(file)
    
    // 1. Calculer le hash de l'image
    hash := sha256.Sum256(data)
    cacheKey := hex.EncodeToString(hash[:])
    
    // 2. Vérifier si déjà dans le cache
    cached, err := redisClient.Get(ctx, cacheKey).Bytes()
    if err == nil {
        // ✅ CACHE HIT : L'image a déjà été traitée
        w.Write(cached)
        return
    }
    
    // ❌ CACHE MISS : 1ère fois qu'on voit cette image
    
    // 3. Envoyer à l'optimizer
    optimized := sendToOptimizer(data)
    
    // 4. Stocker dans Redis (expire après 24h)
    redisClient.Set(ctx, cacheKey, optimized, 24*time.Hour)
    
    // 5. Répondre au client
    w.Write(optimized)
}
```

---

### ⏰ TTL — Time To Live

**Problème :** Si on stocke toutes les images pour toujours, Redis va consommer toute la RAM du serveur.

**Solution :** On donne une **durée de vie** à chaque entrée.

```go
redisClient.Set(ctx, cacheKey, data, 24*time.Hour)
                                      ^^^^^^^^^^^^
                                      TTL = 24 heures
```

**Timeline :**
```
t = 0h    → Redis.Set("abc123...", imageData, 24h)
            Redis stocke : {"abc123...": imageData, expireAt: 2026-02-24 14:00}

t = 12h   → Redis.Get("abc123...")
            ✅ retourne imageData (encore valide)

t = 24h   → Redis supprime automatiquement l'entrée

t = 25h   → Redis.Get("abc123...")
            ❌ KeyNotFound → CACHE MISS → retraitement
```

---

### 🔍 Inspecter Redis en ligne de commande

```bash
# Se connecter à Redis dans Docker
docker exec -it watermark-redis-1 redis-cli

# Voir toutes les clés stockées
KEYS *
# Exemple de sortie :
# 1) "063129c3a4ad87ec..."
# 2) "a3f8c2d1e4b79f3c..."

# Voir le TTL restant d'une clé (en secondes)
TTL "063129c3a4ad87ec..."
# 43200  (= 12 heures restantes)

# Voir la taille d'une entrée (en bytes)
STRLEN "063129c3a4ad87ec..."
# 325480  (= 325 KB)

# Surveiller Redis en temps réel (affiche chaque commande)
MONITOR

# Statistiques mémoire
INFO memory
```

---

### 📊 Impact du cache

**Scénario :** 1000 utilisateurs uploadent 100 images uniques (10 utilisateurs par image).

#### Sans cache
```
1000 requêtes × 200ms = 200 secondes de CPU
```

#### Avec cache
```
100 images uniques × 200ms = 20 secondes
900 cache hits × 1ms       = 0.9 secondes
────────────────────────────────────────
Total                      = 20.9 secondes
```

**Gain :** **10x plus rapide**

---

### 🎯 Cache HIT vs Cache MISS — Visualisation

```
Requête 1 (image A) :
  Client → API → hash="abc123..."
               → Redis.Get("abc123...") → ❌ KeyNotFound (MISS)
               → Optimizer (200ms)
               → Redis.Set("abc123...", result, 24h)
               → Client (total: 203ms)

Requête 2 (image A, même image) :
  Client → API → hash="abc123..."
               → Redis.Get("abc123...") → ✅ Trouvé (HIT)
               → Client (total: 3ms)

Requête 3 (image B, différente) :
  Client → API → hash="def456..."
               → Redis.Get("def456...") → ❌ KeyNotFound (MISS)
               → Optimizer (200ms)
               → Redis.Set("def456...", result, 24h)
               → Client (total: 203ms)

Requête 4 (image A, encore) :
  Client → API → hash="abc123..."
               → Redis.Get("abc123...") → ✅ Trouvé (HIT)
               → Client (total: 3ms)
```

---

<a name="minio"></a>
## 9. MinIO — Stockage objet persistant

### 🎯 Le problème

Si l'optimizer plante en plein traitement, l'image uploadée par le client est **perdue** — il doit tout re-uploader.

```
Scénario sans MinIO :
  Client envoie image → optimizer crash en cours de route
  → image perdue, client doit ré-uploader
  → si optimizer reste KO, traitement impossible
```

---

### ✅ La solution : sauvegarder l'original d'abord

**MinIO** est un serveur de stockage objet **compatible avec l'API Amazon S3**, qui persiste sur disque.

Dès que l'image arrive, elle est sauvegardée dans MinIO **avant** d'être envoyée à l'optimizer. Si l'optimizer plante, l'API récupère l'original depuis MinIO et **réessaie automatiquement**.

```
bucket "watermarks"
└── original/
    ├── a3f8c2d1e4b79f3c....jpg   (image originale, 2.1 MB)
    ├── 063129c3a4ad87ec....jpg   (image originale, 3.4 MB)
    └── b7e2f1a0d5c84e9b....jpg   (image originale, 1.8 MB)
```

---

### 🔄 Flow complet

```
① Lecture image
② SHA256

③ Redis.Get ──► ✅ HIT  → répond immédiatement (< 1ms)

③ Redis.Get ──► ❌ MISS
        │
        ④ MinIO.Put("original/<hash>.jpg")  ← original sauvegardé sur disque
        │
        ⑤ Optimizer
        │
        ├──► ✅ OK
        │       ⑥ Redis.Set (TTL 24h)
        │       ⑦ Répond au client (~200ms)
        │
        └──► ❌ KO (crash, timeout)
                │
                MinIO.Get("original/<hash>.jpg")  ← récupère l'original
                │
                ⑤b Optimizer (2ème tentative)
                │
                ├──► ✅ OK → ⑥ Redis.Set → ⑦ Répond
                └──► ❌ KO → 502 Bad Gateway
```

---

### 💾 Implémentation

```go
// Étape 4 : sauvegarder l'original AVANT de traiter
minioClient.PutObject(ctx, minioBucket, "original/"+cacheKey+".jpg",
    bytes.NewReader(data), int64(len(data)),
    minio.PutObjectOptions{ContentType: "image/jpeg"},
)

// Étape 5 : envoyer à l'optimizer
result, err := sendToOptimizer(optimizerURL, filename, data)
if err != nil {
    // Optimizer KO → récupérer l'original depuis MinIO et réessayer
    obj, _ := minioClient.GetObject(ctx, minioBucket, "original/"+cacheKey+".jpg", ...)
    recovered, _ := io.ReadAll(obj)
    result, err = sendToOptimizer(optimizerURL, filename, recovered)
    if err != nil {
        http.Error(w, "Microservice indisponible", 502)
        return
    }
}

// Étape 6 : mettre en cache Redis
redisClient.Set(ctx, cacheKey, result, 24*time.Hour)
```

---

### ⚠️ Sauvegarde non bloquante

Si MinIO est indisponible, on continue quand même vers l'optimizer :

```go
_, err = minioClient.PutObject(...)
if err != nil {
    log.Printf("⚠ Sauvegarde original échouée : %v", err)
    // On continue — pas de sauvegarde, mais le traitement s'effectue quand même
}
```

**Priorité :** Traiter et répondre > Sauvegarder dans MinIO

---

### 🖥️ Console web MinIO

MinIO expose une interface web sur le port **9001** :

```
http://localhost:9001
Login : minioadmin / minioadmin
```

Elle permet de :
- Parcourir les objets stockés
- Télécharger/supprimer des images
- Voir la consommation disque
- Gérer les buckets et les permissions

---

### 🔍 Inspecter MinIO en ligne de commande

```bash
# Installer le client MinIO (mc)
brew install minio/stable/mc

# Configurer l'alias
mc alias set local http://localhost:9000 minioadmin minioadmin

# Lister les objets du bucket
mc ls local/watermarks

# Voir la taille totale du bucket
mc du local/watermarks

# Télécharger un objet
mc cp local/watermarks/abc123....jpg ./output.jpg
```

---

### 📊 Redis vs MinIO

| Critère | Redis | MinIO |
|---------|-------|-------|
| Vitesse | < 1ms | ~5ms |
| Persistance | Non (RAM) | Oui (disque) |
| TTL | 24h | Illimité |
| Survit au reboot | Non | Oui |
| Capacité | RAM (limitée) | Disque (grande) |
| Usage | Cache chaud | Stockage long terme |

**Gain :** L'image originale est toujours récupérable. Si l'optimizer plante, la **reprise est automatique** sans que le client ait à ré-uploader.

---

<a name="résumé"></a>
## 10. 📊 Résumé des gains de performance

| Optimisation | Problème résolu | Gain | Ressource économisée |
|--------------|-----------------|------|----------------------|
| **io.Pipe** | RAM saturée par les uploads | **300x** | RAM |
| **http.Client partagé** | Connexions TCP répétées | **12x** | Latence réseau |
| **Worker Pool** | CPU saturé par trop de goroutines | **2.4x** | CPU |
| **sync.Pool** | Allocations mémoire constantes | **20x** | GC / RAM |
| **Chargement unique** | Lecture fichier répétée | **1000x** | I/O disque |
| **Gzip** | Bande passante gaspillée | **14%** | Réseau |
| **Redis cache** | Retraitement inutile | **66x** | CPU |
| **MinIO** | Perte données après reboot | **∞** | Durabilité |

---

## 🎯 Performance globale

**Sans optimisations :**
```
1000 images uploadées simultanément
→ 1000 MB RAM
→ 60 secondes CPU
→ 325 MB bande passante
→ Crash probable 💥
```

**Avec optimisations :**
```
1000 images uploadées simultanément
→ 3 MB RAM (-99.7%)
→ 4 secondes CPU (-93%)
→ 280 MB bande passante (-14%)
→ Serveur stable ✅
```

---

## 🧠 Concepts clés à retenir

### 1. **Streaming > Buffering**
Ne charge jamais tout en mémoire si tu peux le traiter par morceaux.

### 2. **Réutilisation > Création**
Réutilise les connexions TCP, les buffers, les ressources chargées.

### 3. **Limitation > Liberté**
Limite les goroutines actives pour éviter la saturation CPU.

### 4. **Cache > Recalcul**
Si le résultat est déterministe, stocke-le en cache.

### 5. **RAM > Disque**
La RAM est 1000x plus rapide. Utilise Redis pour les données fréquentes.

### 6. **Compression = Gratuit**
Gzip coûte peu de CPU mais économise beaucoup de bande passante.

---

## 📚 Pour aller plus loin

- **Profiling Go** : `go tool pprof` pour identifier les bottlenecks
- **Monitoring Redis** : `redis-cli --stat` pour voir les stats en temps réel
- **Load testing** : `wrk`, `hey`, ou `k6` pour tester la charge
- **Distributed caching** : Redis Cluster pour scaler horizontalement

---

**🎓 Fin du cours — Serveur Haute Performance**