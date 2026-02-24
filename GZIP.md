# Cours : gzip
## Compression de données — algorithme, format et usage en HTTP

---

## 📋 Table des matières

1. [C'est quoi gzip ?](#intro)
2. [Le problème sans compression](#probleme)
3. [Comment fonctionne la compression — LZ77 + Huffman](#algorithme)
4. [Le format gzip — ce qu'il y a dans le fichier](#format)
5. [Les niveaux de compression](#niveaux)
6. [gzip en Go — les APIs](#go)
7. [gzip dans HTTP — négociation de contenu](#http)
8. [gzip vs zlib vs deflate — les confusions classiques](#confusion)
9. [Utilisation dans NWS Watermark](#watermark)
10. [Pourquoi gzip est inutile sur du JPEG](#jpeg)
11. [Cas d'usage classiques](#usages)
12. [Résumé](#résumé)

---

<a name="intro"></a>
## 1. C'est quoi gzip ?

**gzip** = **G**NU **zip** — un format de compression de fichiers créé en 1992, standardisé dans la [RFC 1952](https://www.rfc-editor.org/rfc/rfc1952).

C'est aujourd'hui le format de compression le plus utilisé sur le web. Quand ton navigateur reçoit une page HTML compressée, c'est presque toujours du gzip.

**Analogie :** gzip c'est comme écrire un texte en abrégé.
- Au lieu de répéter "Bonjour" 50 fois → on écrit "Bj×50"
- Au lieu de stocker "AAAAAAAAAA" → on stocke "A×10"
- Le destinataire décompresse en relisant les abréviations

```
Données originales  →  [ gzip ]  →  Données compressées (plus petites)
Données compressées →  [ gunzip ] →  Données originales (identiques)
```

La compression est **sans perte** (lossless) : les données décompressées sont **bit pour bit identiques** aux originales.

---

<a name="probleme"></a>
## 2. Le problème sans compression

### Le coût du réseau

Transférer des données sur le réseau a un coût : temps, bande passante, énergie.

```
Sans gzip :
  Serveur ──── 500 KB de HTML ────► Navigateur
               ~500ms sur 8 Mbps

Avec gzip :
  Serveur ──── 80 KB (compressé) ──► Navigateur ──► décompresse en RAM
               ~80ms sur 8 Mbps          (~2ms)
```

**Le gain :** ~5ms de CPU pour compresser → économise 420ms de réseau.
Sur du texte (HTML, JSON, CSS, JS), gzip réduit typiquement la taille de **60 à 80 %**.

### Ce que ça coûte

| Opération | Coût CPU | Gain réseau |
|---|---|---|
| Compression (serveur) | ~1-5 ms | -60 à -80 % sur du texte |
| Décompression (client) | < 1 ms | — |

La décompression est quasi gratuite. La compression coûte un peu de CPU — c'est pour ça que le niveau de compression est réglable.

---

<a name="algorithme"></a>
## 3. Comment fonctionne la compression — LZ77 + Huffman

gzip utilise deux algorithmes en série : **LZ77** puis **Huffman coding**. Ensemble ils forment l'algorithme **DEFLATE**.

---

### Étape 1 — LZ77 : éliminer les répétitions

LZ77 (Lempel-Ziv 1977) repère les **séquences qui se répètent** dans les données et les remplace par une **référence** vers l'occurrence précédente.

**Exemple :**
```
Texte original :
"le chat mange le chat mange le poisson"

LZ77 trouve que "le chat mange " se répète :
"le chat mange [référence: recule 15 chars, copie 15] le poisson"

Résultat : beaucoup moins de données à stocker
```

Formellement, chaque référence = `(distance, longueur)` :
- `distance` = combien de caractères en arrière aller chercher
- `longueur` = combien de caractères copier

```
Original  : ABCDEABCDE
LZ77      : ABCDE(5,5)   ← "recule 5, copie 5"
Taille    : 5 chars + 1 token  au lieu de 10 chars
```

LZ77 maintient une **fenêtre glissante** (32 KB dans DEFLATE) : il ne peut référencer que les 32 derniers KB de données. C'est pour ça que compresser un fichier entier est plus efficace que compresser des petits morceaux séparément.

---

### Étape 2 — Huffman : encoder intelligemment

Après LZ77, les données sont constituées de symboles (caractères + références). L'encodage de Huffman assigne des **codes binaires courts aux symboles fréquents** et des codes longs aux symboles rares.

**Analogie :** le morse. Les lettres fréquentes (E, T) ont un code court (`.`, `-`), les rares (Q, Z) ont un code long. Huffman fait pareil mais de façon optimale et automatique.

```
Fréquences dans le texte :
  'A' → 40%   → code: 0        (1 bit)
  'B' → 30%   → code: 10       (2 bits)
  'C' → 20%   → code: 110      (3 bits)
  'D' → 10%   → code: 111      (3 bits)

Sans Huffman : chaque lettre = 8 bits (ASCII)
Avec Huffman : moyenne = 0.4×1 + 0.3×2 + 0.2×3 + 0.1×3 = 1.9 bits/lettre
Gain : 8 → 1.9 bits = compression 76%
```

L'arbre de Huffman est calculé à la volée pour chaque bloc de données et stocké dans le fichier compressé (pour que le décompresseur puisse reconstruire les codes).

---

### DEFLATE = LZ77 + Huffman

```
Données brutes
    │
    ▼ LZ77
Données sans répétitions (références + littéraux)
    │
    ▼ Huffman
Bits compressés
    │
    ▼ + header gzip + checksum
Fichier .gz
```

---

<a name="format"></a>
## 4. Le format gzip — ce qu'il y a dans le fichier

Un fichier `.gz` n'est pas juste des données compressées. Il a une structure précise définie par la RFC 1952 :

```
┌─────────────────────────────────────────────────────────────┐
│                       FICHIER GZIP                          │
├──────────┬─────────────────────────────┬────────────────────┤
│  HEADER  │         DONNÉES             │      FOOTER        │
│ (10 oct) │    (DEFLATE compressé)      │     (8 octets)     │
└──────────┴─────────────────────────────┴────────────────────┘
```

### Le header (10 octets minimum)

```
Offset  Taille  Contenu
0       2       Magic number : 0x1f 0x8b  (identifie un fichier gzip)
2       1       Méthode de compression : 0x08 = DEFLATE (toujours)
3       1       Flags (nom de fichier, commentaire, etc.)
4       4       Timestamp de modification (Unix epoch, little-endian)
8       1       Niveau de compression (hint, non normatif)
9       1       OS source (0=FAT, 3=Unix, 11=NTFS, 255=inconnu)
```

Les deux premiers octets `0x1f 0x8b` sont le **magic number** — c'est comme une signature qui permet à n'importe quel outil de savoir qu'il lit un fichier gzip, peu importe l'extension.

### Le footer (8 octets)

```
Offset  Taille  Contenu
-8      4       CRC32 des données originales (vérifie l'intégrité)
-4      4       Taille des données originales (modulo 2^32)
```

**Le CRC32** est un checksum : après décompression, le décompresseur recalcule le CRC32 et le compare. Si ça ne correspond pas → les données sont corrompues → erreur.

---

<a name="niveaux"></a>
## 5. Les niveaux de compression

gzip propose 10 niveaux, de 0 (aucune compression) à 9 (compression maximale).

| Niveau | Nom Go | Vitesse | Taille | Usage |
|---|---|---|---|---|
| 0 | `gzip.NoCompression` | Max | 100% (+ header) | Jamais utile |
| 1 | `gzip.BestSpeed` | Très rapide | ~65% | Streaming temps réel ✅ |
| 6 | `gzip.DefaultCompression` | Moyen | ~58% | Cas général ✅ |
| 9 | `gzip.BestCompression` | Lent | ~55% | Fichiers statiques |

### Le compromis vitesse / taille

```
Niveau 1 :  ████░░░░░░  compression  ██████████  vitesse   → streaming
Niveau 6 :  ██████░░░░  compression  ████░░░░░░  vitesse   → API
Niveau 9 :  ████████░░  compression  ██░░░░░░░░  vitesse   → assets statiques
```

**Le niveau 6 est le défaut** car il offre ~95% du gain maximal en ~50% du temps de niveau 9.

**Le niveau 1 (`BestSpeed`)** est utilisé pour le streaming HTTP où chaque milliseconde compte — la taille finale est légèrement plus grande mais la latence est minimale.

### Pourquoi pas toujours le niveau 9 ?

Pour un fichier de 500 KB :
```
Niveau 1 : 2ms de CPU → 320 KB résultat
Niveau 9 : 15ms de CPU → 275 KB résultat
Différence : +13ms CPU pour -45 KB → souvent pas rentable en temps réel
```

---

<a name="go"></a>
## 6. gzip en Go — les APIs

Le package `compress/gzip` de la bibliothèque standard fournit deux types principaux.

### Compression — gzip.Writer

```go
import "compress/gzip"

// Créer un writer avec le niveau par défaut (6)
gz := gzip.NewWriter(destination)

// Créer un writer avec un niveau spécifique
gz, err := gzip.NewWriterLevel(destination, gzip.BestSpeed)

// Écrire des données (les compresse à la volée)
gz.Write([]byte("données à compresser"))

// IMPORTANT : toujours fermer pour vider le buffer interne et écrire le footer
gz.Close()
```

`gzip.Writer` implémente `io.Writer` — on peut l'utiliser partout où un `io.Writer` est attendu.

### Décompression — gzip.Reader

```go
// Créer un reader depuis une source compressée
gr, err := gzip.NewReader(source)
if err != nil {
    // source n'est pas du gzip valide
}
defer gr.Close()

// Lire les données décompressées
data, err := io.ReadAll(gr)
```

`gzip.Reader` implémente `io.Reader`.

### Exemple complet — compresser/décompresser en mémoire

```go
// ── Compression ──────────────────────────────────────────
var buf bytes.Buffer
gz, _ := gzip.NewWriterLevel(&buf, gzip.BestSpeed)
gz.Write([]byte("Bonjour monde ! Bonjour monde ! Bonjour monde !"))
gz.Close()

fmt.Printf("Original : 48 octets → Compressé : %d octets\n", buf.Len())

// ── Décompression ─────────────────────────────────────────
gr, _ := gzip.NewReader(&buf)
defer gr.Close()
original, _ := io.ReadAll(gr)
fmt.Printf("Décompressé : %s\n", original)
```

### La nécessité de Close()

`gzip.Writer` bufferise les données et les compresse par blocs. Appeler `Close()` :
1. Vide le dernier bloc (flush)
2. Écrit le footer (CRC32 + taille originale)

Sans `Close()`, le fichier gzip est **tronqué et invalide** — le décompresseur ne trouvera pas le footer et retournera une erreur.

```go
// ❌ Oubli de Close() → fichier gzip invalide
gz := gzip.NewWriter(w)
gz.Write(data)
// gz.Close() oublié → le reader recevra une erreur "unexpected EOF"

// ✅ Toujours fermer
gz := gzip.NewWriter(w)
defer gz.Close()  // s'exécute même si une erreur survient avant
gz.Write(data)
```

---

<a name="http"></a>
## 7. gzip dans HTTP — négociation de contenu

### Comment le navigateur demande la compression

Le navigateur annonce qu'il sait décompresser gzip via le header `Accept-Encoding` :

```
GET /image/abc123 HTTP/1.1
Host: localhost:3000
Accept-Encoding: gzip, deflate, br
```

Le serveur peut alors répondre avec du contenu compressé :

```
HTTP/1.1 200 OK
Content-Type: image/jpeg
Content-Encoding: gzip
Content-Length: 45231

[données compressées]
```

Le navigateur lit `Content-Encoding: gzip`, décompresse, et obtient le JPEG original.

### Les headers clés

| Header | Sens | Exemple |
|---|---|---|
| `Accept-Encoding` | Client → Serveur : "j'accepte ces compressions" | `gzip, deflate, br` |
| `Content-Encoding` | Serveur → Client : "j'ai utilisé cette compression" | `gzip` |
| `Transfer-Encoding` | Encodage du transport (chunked) | `chunked` |

**`Content-Encoding` vs `Transfer-Encoding`** — une confusion fréquente :
- `Content-Encoding` : compression **du contenu** (gzip) → le client décompresse
- `Transfer-Encoding` : encodage **du transport** (chunked) → HTTP gère automatiquement

### La négociation est optionnelle

Le serveur n'est **pas obligé** de compresser même si le client le demande. Il peut répondre sans `Content-Encoding` et le client accepte les données brutes.

```go
// Vérification côté serveur
if strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
    // le client supporte gzip → on compresse
} else {
    // on envoie brut
}
```

---

<a name="confusion"></a>
## 8. gzip vs zlib vs deflate — les confusions classiques

Ces trois termes désignent des choses liées mais différentes. La confusion est historique et quasi universelle.

```
DEFLATE (algorithme)
    │
    ├── zlib   (format : header zlib 2 octets + DEFLATE + Adler32)
    │          utilisé en interne par PNG, ZIP, HTTP/2
    │
    └── gzip   (format : header gzip 10 octets + DEFLATE + CRC32)
               utilisé en HTTP, fichiers .gz, tar.gz
```

| | DEFLATE | zlib | gzip |
|---|---|---|---|
| Nature | Algorithme | Format de fichier | Format de fichier |
| Header | Aucun | 2 octets | 10 octets |
| Checksum | Aucun | Adler32 | CRC32 |
| Magic number | — | `0x78 0x9C` | `0x1f 0x8b` |
| Usage typique | Interne | PNG, ZIP | HTTP, fichiers |

### Le piège du header HTTP `deflate`

En HTTP, le header `Accept-Encoding: deflate` ne signifie **pas** "DEFLATE brut" — en pratique les navigateurs envoient du **zlib** (DEFLATE avec header zlib). C'est une erreur historique dans la spécification HTTP/1.1 jamais corrigée.

**Conséquence pratique :** toujours utiliser `gzip` en HTTP, jamais `deflate`.

### Brotli (br) — le successeur moderne

`br` dans `Accept-Encoding: gzip, deflate, br` est **Brotli**, développé par Google en 2015.
- Compression ~15-25% meilleure que gzip
- Décompression aussi rapide
- Mais : compression **beaucoup plus lente** → principalement pour les assets statiques pré-compressés
- Non supporté par tous les clients (Go standard library ne l'inclut pas)

---

<a name="watermark"></a>
## 9. Utilisation dans NWS Watermark

Dans `api/main.go`, gzip est utilisé dans `sendResponse` pour compresser l'image avant de la renvoyer au client :

```go
func sendResponse(w http.ResponseWriter, r *http.Request, data []byte) {
    w.Header().Set("Content-Type", "image/jpeg")

    if strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
        w.Header().Set("Content-Encoding", "gzip")

        // BestSpeed (niveau 1) : on privilégie la vitesse sur le taux de compression
        // car on est sur du chemin chaud (réponse HTTP temps réel)
        gz, err := gzip.NewWriterLevel(w, gzip.BestSpeed)
        if err != nil {
            http.Error(w, "Erreur compression", http.StatusInternalServerError)
            return
        }
        defer gz.Close()  // écrit le footer gzip + vide le buffer
        gz.Write(data)
    } else {
        w.Write(data)
    }
}
```

### Le flux de données

```
data []byte (image JPEG en RAM)
    │
    ▼ gz.Write(data)
gzip.Writer (niveau 1, BestSpeed)
    │  compresse à la volée
    ▼ gz.Close() → flush + footer CRC32
http.ResponseWriter (w)
    │  Header: Content-Encoding: gzip
    ▼ TCP réseau
Navigateur
    │  lit Content-Encoding: gzip → décompresse automatiquement
    ▼ affiche l'image JPEG
```

### Pourquoi écrire directement dans w ?

`gzip.NewWriterLevel(w, ...)` prend `w` (le `http.ResponseWriter`) comme destination.
Quand `gz.Write(data)` est appelé, les données compressées partent **directement** dans la réponse HTTP, sans buffer intermédiaire.

C'est l'équivalent de `io.Pipe` mais en sens inverse : au lieu de brancher un Writer sur un Reader, on branche le compresseur directement sur la sortie réseau.

```
                     Même processus
┌────────────────────────────────────────┐
│                                        │
│  data []byte  →  gzip.Writer  →  w    │──── TCP ────► Navigateur
│               (compresse)     (réseau) │
└────────────────────────────────────────┘
```

### Pourquoi BestSpeed et pas DefaultCompression ?

```
Niveau 1 (BestSpeed)      : ~2ms  → image de 500KB → 490KB (JPEG peu compressible)
Niveau 6 (Default)        : ~8ms  → image de 500KB → 488KB
Différence de gain        : 2 KB
Différence de temps CPU   : 6ms

→ 6ms de CPU perdu pour 2 KB économisés sur du JPEG → pas rentable
→ BestSpeed est le bon choix ici
```

---

<a name="jpeg"></a>
## 10. Pourquoi gzip est (presque) inutile sur du JPEG

C'est le paradoxe de NWS Watermark : on applique gzip sur des JPEG, mais le gain est quasi nul.

### JPEG est déjà compressé

JPEG utilise sa propre compression interne :
1. **DCT** (Discrete Cosine Transform) — transforme les blocs de pixels en fréquences
2. **Quantization** — réduit la précision des hautes fréquences (perte)
3. **Huffman coding** — le même algorithme que gzip, appliqué aux coefficients DCT

```
Données brutes image :  3840 × 2160 × 3 octets = 24 MB
Après JPEG (qualité 85) :                          2-5 MB
Après gzip sur le JPEG :                           1.9-4.9 MB  (≈ 0-2% de gain)
```

**gzip ne trouve presque rien à compresser** dans un JPEG parce que les données sont déjà pseudo-aléatoires après la compression JPEG.

### Alors pourquoi l'appliquer quand même ?

1. **Cohérence** : l'API compresse toutes ses réponses de la même façon, peu importe le contenu
2. **Gain non nul** : même 1-2% sur une image de 2 MB = 20-40 KB économisés
3. **Le header JPEG** et les métadonnées EXIF, eux, sont compressibles
4. **Coût quasi nul** avec BestSpeed : ~2ms pour presque rien à faire

### Pour les formats vraiment compressibles

| Format | Gain gzip typique |
|---|---|
| HTML | 70-80% |
| JSON | 60-75% |
| CSS | 60-70% |
| JavaScript | 50-65% |
| PNG | 0-5% (déjà compressé avec DEFLATE) |
| JPEG | 0-2% (déjà compressé) |
| WebP | 0-3% (déjà compressé) |
| Vidéo MP4 | 0-1% |

**Règle :** ne pas compresser ce qui est déjà compressé. Dans un vrai système de production, on filtrerait les Content-Types avant d'appliquer gzip.

---

<a name="usages"></a>
## 11. Cas d'usage classiques

### 1. Compresser une réponse HTTP (notre cas)

```go
func handler(w http.ResponseWriter, r *http.Request) {
    data := genererJSON()  // 500 KB de JSON

    if strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
        w.Header().Set("Content-Encoding", "gzip")
        gz, _ := gzip.NewWriterLevel(w, gzip.DefaultCompression)
        defer gz.Close()
        gz.Write(data)
    } else {
        w.Write(data)
    }
}
```

### 2. Compresser un fichier sur disque

```go
src, _ := os.Open("data.json")
defer src.Close()

dst, _ := os.Create("data.json.gz")
defer dst.Close()

gz, _ := gzip.NewWriterLevel(dst, gzip.BestCompression)
defer gz.Close()  // IMPORTANT : avant de fermer dst

io.Copy(gz, src)  // lit depuis src, compresse, écrit dans dst
```

### 3. Lire un fichier gzip

```go
f, _ := os.Open("data.json.gz")
defer f.Close()

gr, _ := gzip.NewReader(f)
defer gr.Close()

data, _ := io.ReadAll(gr)
// data contient le JSON original décompressé
```

### 4. Compression à la volée avec io.Pipe

Combiner gzip et io.Pipe pour compresser en streaming sans buffer intermédiaire :

```go
pr, pw := io.Pipe()

go func() {
    gz, _ := gzip.NewWriterLevel(pw, gzip.BestSpeed)
    io.Copy(gz, source)  // lit depuis source, compresse dans pw
    gz.Close()           // flush + footer gzip
    pw.Close()           // signale EOF
}()

http.Post(url, "application/gzip", pr)  // lit depuis pr → réseau
```

### 5. Middleware gzip générique

```go
func gzipMiddleware(next http.Handler) http.Handler {
    return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        if !strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
            next.ServeHTTP(w, r)
            return
        }

        w.Header().Set("Content-Encoding", "gzip")
        gz, _ := gzip.NewWriterLevel(w, gzip.DefaultCompression)
        defer gz.Close()

        // On remplace le ResponseWriter par un wrapper qui écrit dans gz
        next.ServeHTTP(&gzipResponseWriter{ResponseWriter: w, gz: gz}, r)
    })
}
```

---

<a name="résumé"></a>
## 12. Résumé

### Ce que fait gzip

```
Données texte (HTML, JSON...)  →  gzip  →  -60 à -80% de taille
Données déjà compressées (JPEG, PNG...)  →  gzip  →  -0 à -2% (inutile)
```

### Les deux algorithmes internes

| Algorithme | Rôle | Gain |
|---|---|---|
| LZ77 | Supprime les répétitions → références (distance, longueur) | Variable selon le contenu |
| Huffman | Encode les symboles fréquents sur moins de bits | ~20-30% supplémentaire |

### Le format d'un fichier gzip

```
[ Header 10 oct ] [ Données DEFLATE ] [ Footer 8 oct ]
  magic: 0x1f8b     LZ77 + Huffman     CRC32 + taille
```

### Les APIs Go à retenir

```go
// Compresser
gz, _ := gzip.NewWriterLevel(destination, gzip.BestSpeed)
defer gz.Close()  // NE PAS OUBLIER → écrit le footer
gz.Write(data)

// Décompresser
gr, _ := gzip.NewReader(source)
defer gr.Close()
data, _ := io.ReadAll(gr)
```

### Les règles à retenir

1. **Toujours `Close()`** — sans ça, le footer n'est pas écrit et le fichier est invalide
2. **BestSpeed pour le temps réel** — niveau 1 pour les API HTTP sous charge
3. **DefaultCompression pour les assets** — niveau 6 pour les fichiers statiques
4. **Pas sur du JPEG/PNG/WebP** — déjà compressés, gzip n'apporte rien
5. **Toujours `Accept-Encoding` avant** — ne pas compresser si le client ne le supporte pas
6. **`Content-Encoding: gzip`** — l'oublier = le navigateur reçoit du binaire illisible

### Comparaison rapide des niveaux

| Niveau | Nom | Temps (500 KB) | Taille | Utilisation |
|---|---|---|---|---|
| 1 | `BestSpeed` | ~2ms | ~65% | API HTTP temps réel ✅ |
| 6 | `DefaultCompression` | ~8ms | ~58% | Cas général ✅ |
| 9 | `BestCompression` | ~20ms | ~55% | Assets pré-compressés |
