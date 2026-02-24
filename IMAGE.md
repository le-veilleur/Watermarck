# Cours : Optimisation Image
## WebP, AVIF, traitement parallèle, formats et pipelines

---

## 📋 Table des matières

1. [C'est quoi l'optimisation image ?](#intro)
2. [Les formats — JPEG, PNG, WebP, AVIF](#formats)
3. [Négociation de format via HTTP Accept](#negociation)
4. [Résolution et resize — algorithmes d'interpolation](#resize)
5. [Traitement parallèle des pixels](#parallel)
6. [Progressive JPEG — affichage progressif](#progressive)
7. [Lazy decoding — lire sans décoder](#lazy)
8. [Métadonnées EXIF — taille cachée](#exif)
9. [Qualité adaptative — choisir dynamiquement](#qualite)
10. [Pipeline d'optimisation complet](#pipeline)
11. [libjpeg-turbo — JPEG rapide via cgo](#libjpeg)
12. [Utilisation dans NWS Watermark](#watermark)
13. [Résumé](#résumé)

---

<a name="intro"></a>
## 1. C'est quoi l'optimisation image ?

Les images sont souvent **la ressource la plus lourde** d'une application web. Optimiser les images c'est :

1. **Choisir le bon format** (WebP vs JPEG vs AVIF)
2. **Redimensionner** (ne pas envoyer une image 4K à un mobile)
3. **Compresser** (trouver le meilleur ratio qualité/poids)
4. **Transformer** (watermark, recadrage, filtres)
5. **Délivrer** (CDN, lazy loading, formats modernes)

**Impact concret :**

```
Image originale :  4000×3000 JPEG = 8 MB
Après optimisation : 1920×1080 WebP qualité 80 = 180 KB
Gain : 97.8% de réduction → page 44x plus rapide à charger
```

---

<a name="formats"></a>
## 2. Les formats — JPEG, PNG, WebP, AVIF

### JPEG (1992)

- Compression avec perte (lossy) — DCT + Huffman
- Idéal pour les photos (dégradés, couleurs naturelles)
- Mauvais pour le texte, les lignes nettes, les transparences
- Support universel (100% des navigateurs)

```
JPEG qualité 85 : bon compromis taille/qualité pour les photos
JPEG qualité 60 : compression agressive, artefacts visibles sur les coins nets
JPEG qualité 95 : quasi-lossless mais 3x plus lourd que qualité 85
```

### PNG (1996)

- Compression sans perte (lossless) — DEFLATE (zlib)
- Supporte la transparence (canal alpha)
- Idéal pour les logos, icônes, captures d'écran, images avec texte
- Mauvais pour les photos (fichiers énormes)

```
PNG-24 : couleurs + transparence → gros fichiers
PNG-8  : 256 couleurs indexées → plus petit mais limité
```

### WebP (2010, Google)

- Deux modes : lossy (basé sur VP8) et lossless (basé sur VP8L)
- **30-35% plus léger que JPEG** à qualité visuelle équivalente
- Supporte la transparence (même en lossy)
- Support navigateur : 97% (IE non supporté, mais IE est mort)

```
JPEG qualité 85 : 500 KB → WebP qualité 80 : ~340 KB (même qualité perçue)
```

### AVIF (2019, Alliance for Open Media)

- Basé sur le codec vidéo AV1 (Netflix, Google, Apple...)
- **50% plus léger que JPEG** à qualité équivalente
- Meilleure qualité sur les zones uniformes et les dégradés
- Compression lente (10-100x plus lente que JPEG) → préférer la pré-compression
- Support navigateur : 90% (Safari 16+, Chrome 85+, Firefox 93+)

### JPEG XL (2021)

- Le futur potentiel : 60% plus léger que JPEG
- Ré-encodage JPEG sans perte possible (utile pour migrer l'existant)
- Support navigateur : 75% (Chrome 91+ derrière flag, Firefox 90+)
- Retiré temporairement de Chrome en 2022, réintégré en 2024

### Comparaison pour une photo 1920×1080

| Format | Taille | Qualité | Compression | Transparence | Support |
|---|---|---|---|---|---|
| JPEG | 500 KB | Référence | Lossy | Non | 100% |
| PNG | 2.1 MB | Parfaite | Lossless | Oui | 100% |
| WebP | 340 KB (-32%) | ≈ JPEG | Lossy/Lossless | Oui | 97% |
| AVIF | 250 KB (-50%) | ≥ JPEG | Lossy | Oui | 90% |
| JPEG XL | 200 KB (-60%) | ≥ JPEG | Lossy/Lossless | Oui | 75% |

**Stratégie en production :**
```
AVIF  → si supporté par le navigateur  (meilleur ratio)
WebP  → sinon, si supporté            (très bon ratio, support large)
JPEG  → fallback universel            (100% support)
```

---

<a name="negociation"></a>
## 3. Négociation de format via HTTP Accept

Le navigateur annonce les formats qu'il accepte dans le header `Accept` :

```
Accept: image/avif,image/webp,image/apng,image/svg+xml,image/*,*/*;q=0.8
```

Le serveur choisit le meilleur format supporté :

```go
func bestImageFormat(r *http.Request) string {
    accept := r.Header.Get("Accept")
    switch {
    case strings.Contains(accept, "image/avif"):
        return "avif"
    case strings.Contains(accept, "image/webp"):
        return "webp"
    default:
        return "jpeg"
    }
}

func encodeImage(img image.Image, format string) ([]byte, string, error) {
    var buf bytes.Buffer
    var contentType string

    switch format {
    case "webp":
        // github.com/chai2010/webp
        err := webp.Encode(&buf, img, &webp.Options{Lossless: false, Quality: 80})
        contentType = "image/webp"
        return buf.Bytes(), contentType, err

    case "avif":
        // github.com/gen2brain/avif
        err := avif.Encode(&buf, img, avif.Options{Quality: 60, Speed: 6})
        contentType = "image/avif"
        return buf.Bytes(), contentType, err

    default: // jpeg
        err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: quality})
        contentType = "image/jpeg"
        return buf.Bytes(), contentType, err
    }
}
```

### Cache key avec le format

```go
// La cache key doit inclure le format pour éviter de servir du WebP à un client qui demande JPEG
format := bestImageFormat(r)
hashInput := append(data, []byte(wmText+"|"+wmPosition+"|"+format)...)
cacheKey := sha256.Sum256(hashInput)
```

### Vary header — indiquer au CDN de cacher par format

```go
// Sans Vary : un CDN pourrait mettre en cache du WebP et le servir à un navigateur qui veut JPEG
w.Header().Set("Vary", "Accept")  // "le contenu varie selon le header Accept"
```

---

<a name="resize"></a>
## 4. Résolution et resize — algorithmes d'interpolation

Quand on redimensionne une image, on doit **interpoler** les pixels — estimer la valeur des pixels qui n'existaient pas dans l'original.

### Les algorithmes

**Nearest Neighbor (voisin le plus proche)**
```
Algo : pixel cible = pixel source le plus proche
Vitesse : ⚡⚡⚡⚡⚡ (le plus rapide)
Qualité : ⭐ (pixelisé, "effet minecraft")
Usage   : icônes pixel art, miniatures très petites
```

**Bilinear (utilisé dans notre optimizer)**
```
Algo : moyenne pondérée des 4 pixels voisins
Vitesse : ⚡⚡⚡⚡
Qualité : ⭐⭐⭐ (bon pour les agrandissements modérés)
Usage   : redimensionnement général, temps réel
```

**Bicubic**
```
Algo : polynôme cubique sur les 16 pixels voisins
Vitesse : ⚡⚡
Qualité : ⭐⭐⭐⭐ (meilleur pour les agrandissements)
Usage   : photos, impression
```

**Lanczos**
```
Algo : filtre sinc fenêtré (mathématiquement optimal)
Vitesse : ⚡
Qualité : ⭐⭐⭐⭐⭐ (meilleure qualité)
Usage   : réduction haute qualité, publication
```

**CatmullRom (disponible dans x/image/draw)**
```
Algo : spline cubique hermitienne
Vitesse : ⚡⚡
Qualité : ⭐⭐⭐⭐ (bon compromis bicubic/lanczos)
Usage   : bonne alternative à Lanczos plus rapide
```

### Comparaison visuelle

```
Original 4K → réduction à 1080p

Nearest : ██████ rapide, qualité médiocre (aliasing visible)
Bilinear : ████░░ rapide, qualité correcte, léger flou
Bicubic  : ██░░░░ moyen, qualité bonne, plus net que bilinear
Lanczos  : █░░░░░ lent, meilleure qualité, peut créer des halos
```

### Choisir selon le contexte

```go
switch useCase {
case "thumbnail":    // miniature pour liste
    xdraw.NearestNeighbor.Scale(dst, dst.Bounds(), img, img.Bounds(), xdraw.Over, nil)
case "preview":      // preview en temps réel (notre cas)
    xdraw.BiLinear.Scale(dst, dst.Bounds(), img, img.Bounds(), xdraw.Over, nil)
case "export":       // export qualité maximale
    xdraw.CatmullRom.Scale(dst, dst.Bounds(), img, img.Bounds(), xdraw.Over, nil)
}
```

### Resize avec préservation du ratio (amélioration)

```go
// Calculer les nouvelles dimensions en couvrant maxWidth×maxHeight
// sans déformer l'image ni laisser de bandes noires
func resizeDimensions(w, h, maxW, maxH int) (int, int) {
    if w <= maxW && h <= maxH {
        return w, h  // déjà dans les limites
    }

    // Scale factor le plus restrictif
    scaleW := float64(maxW) / float64(w)
    scaleH := float64(maxH) / float64(h)
    scale  := math.Min(scaleW, scaleH)

    return int(float64(w) * scale), int(float64(h) * scale)
}
```

---

<a name="parallel"></a>
## 5. Traitement parallèle des pixels

### Le bottleneck actuel

```go
// sampleLuminance : 200×50 = 10 000 pixels, traités séquentiellement
for py := startY; py < endY; py++ {
    for px := startX; px < endX; px++ {
        r, g, b, _ := img.At(px, py).RGBA()
        total += 0.299*float64(r>>8) + 0.587*float64(g>>8) + 0.114*float64(b>>8)
        count++
    }
}
```

Pour une image complexe, `applyWatermark` + `sampleLuminance` peut prendre 50-200ms.

### Parallélisation par lignes avec goroutines

```go
func sampleLuminanceParallel(img image.Image, x, y int) float64 {
    bounds := img.Bounds()
    startX := x
    startY := max(y-sampleH, bounds.Min.Y)
    endX   := min(startX+sampleW, bounds.Max.X)
    endY   := min(startY+sampleH, bounds.Max.Y)

    rows := endY - startY
    totals := make([]float64, rows)  // une entrée par ligne → pas de contention

    var wg sync.WaitGroup
    for i, py := 0, startY; py < endY; i, py = i+1, py+1 {
        wg.Add(1)
        go func(row, idx int) {
            defer wg.Done()
            var rowTotal float64
            for px := startX; px < endX; px++ {
                r, g, b, _ := img.At(px, row).RGBA()
                rowTotal += 0.299*float64(r>>8) + 0.587*float64(g>>8) + 0.114*float64(b>>8)
            }
            totals[idx] = rowTotal  // écriture isolée par index → pas de mutex
        }(py, i)
    }
    wg.Wait()

    var total float64
    for _, t := range totals { total += t }
    return total / float64((endX-startX)*(endY-startY))
}
```

### Quand la parallélisation aide vs nuit

**La parallélisation est utile si :**
- Le travail par goroutine est significatif (> ~1µs)
- Les données sont indépendantes (pas de dépendances entre lignes)
- On a plusieurs CPU disponibles

**La parallélisation nuit si :**
- Le travail est trop petit → overhead de création de goroutine > gain
- Contention sur un mutex partagé → sérialisation forcée
- Cache L1/L2 CPU thrashing (chaque goroutine évince les données des autres)

```
Pour sampleLuminance (10 000 pixels) :
  Séquentiel : ~50µs
  4 goroutines : ~20µs (gain x2.5, pas x4 à cause de l'overhead)
  50 goroutines : ~80µs (pire ! overhead > gain)

Règle pratique : 1 goroutine par CPU, pas plus
```

### Traitement d'images en batch — pipeline parallèle

```go
// Traiter N images en parallèle avec un worker pool
func processBatch(images []image.Image, numWorkers int) []image.Image {
    jobs    := make(chan image.Image, len(images))
    results := make(chan image.Image, len(images))

    // Démarrer les workers
    for i := 0; i < numWorkers; i++ {
        go func() {
            for img := range jobs {
                resized   := resize(img)
                watermarked, _ := applyWatermark(resized, "NWS © 2026", "bottom-right")
                results <- watermarked
            }
        }()
    }

    // Envoyer les jobs
    for _, img := range images {
        jobs <- img
    }
    close(jobs)

    // Collecter les résultats
    processed := make([]image.Image, len(images))
    for i := range processed {
        processed[i] = <-results
    }
    return processed
}
```

---

<a name="progressive"></a>
## 6. Progressive JPEG — affichage progressif

### JPEG baseline vs JPEG progressif

**JPEG baseline (notre cas actuel) :**
```
Chargement : ligne 1, ligne 2, ligne 3... → l'image s'affiche de haut en bas
→ Si la connexion est lente → on voit une image à moitié chargée
```

**JPEG progressif :**
```
Passe 1 : image complète mais floue (basse fréquence DCT)
Passe 2 : image plus nette
Passe 3 : qualité finale
→ Si la connexion est lente → on voit l'image entière dès le début, qui se précise
```

Pour les images < 10 KB : baseline est plus léger.
Pour les images > 10 KB : progressif est ~20% plus léger ET meilleure expérience utilisateur.

### Encodage progressif en Go

La stdlib `image/jpeg` ne supporte pas le JPEG progressif en écriture. Il faut passer par `libjpeg-turbo` via cgo ou une librairie externe.

```go
// Option 1 : librairie pure Go (moins performante mais pas de cgo)
// github.com/disintegration/imaging
import "github.com/disintegration/imaging"
imaging.Save(img, "output.jpg", imaging.JPEGQuality(85))
// Note : ne supporte pas non plus le progressif en natif

// Option 2 : cgo + libjpeg-turbo (voir section libjpeg-turbo)
// Supporte le progressif nativement
```

---

<a name="lazy"></a>
## 7. Lazy decoding — lire sans décoder

### Le problème

Pour valider une image uploadée (dimensions, format), on n'a pas besoin de décoder **tous** les pixels. Décoder un JPEG 4K complet juste pour vérifier ses dimensions = gaspillage.

### DecodeConfig — headers seulement

```go
// Lit UNIQUEMENT le header de l'image → dimensions et format
// Ne décode pas les pixels → très rapide
config, format, err := image.DecodeConfig(file)
if err != nil {
    http.Error(w, "Format invalide", http.StatusBadRequest)
    return
}

log.Info().
    Str("format", format).
    Int("width", config.Width).
    Int("height", config.Height).
    Msg("image validated")

// Valider avant de faire le traitement complet
if config.Width > 8000 || config.Height > 8000 {
    http.Error(w, "Image trop grande (max 8000×8000)", http.StatusBadRequest)
    return
}

// Revenir au début du fichier pour le décodage complet
file.Seek(0, io.SeekStart)
img, _, err := image.Decode(file)
```

---

<a name="exif"></a>
## 8. Métadonnées EXIF — taille cachée

**EXIF** (Exchangeable Image File Format) = métadonnées embarquées dans les JPEG : appareil photo, GPS, date, exposition, orientation...

### Pourquoi c'est important

```
Photo iPhone : 3.2 MB
  - Pixels JPEG  : 2.8 MB (87.5%)
  - EXIF         : 400 KB (12.5%) ← données GPS, miniature embarquée, profil ICC
```

Supprimer les EXIF = gagner 5-15% de poids sans aucune perte de qualité visuelle.

**Sécurité :** les données EXIF peuvent contenir des coordonnées GPS précises. Servir des photos avec EXIF = divulguer la localisation de l'utilisateur.

### Stripper les EXIF en Go

```go
// La stdlib image/jpeg ne préserve pas les EXIF lors du ré-encodage
// → un simple decode/encode supprime déjà les EXIF

img, _, err := image.Decode(input)   // décode → perd les EXIF
jpeg.Encode(output, img, opts)       // ré-encode → sans EXIF

// Pour lire les EXIF avant de les supprimer :
// github.com/rwcarlsen/goexif/exif
x, err := exif.Decode(file)
lat, lng, _ := x.LatLong()
log.Info().Float64("lat", lat).Float64("lng", lng).Msg("image gps")
file.Seek(0, io.SeekStart)
```

---

<a name="qualite"></a>
## 9. Qualité adaptative — choisir dynamiquement

Au lieu d'une qualité fixe (85), adapter selon la taille de l'image et l'usage :

```go
func adaptiveQuality(w, h int, targetFormat string) int {
    pixels := w * h

    switch {
    case pixels < 100*100:       // miniature < 100×100
        return 70
    case pixels < 500*500:       // preview
        return 80
    case pixels < 1920*1080:     // HD
        return 85
    default:                     // 4K+
        return 90   // plus de détails à préserver
    }
}

// AVIF et WebP ont une échelle de qualité différente de JPEG
// JPEG 85 ≈ WebP 80 ≈ AVIF 60  (en qualité perçue)
func qualityForFormat(jpegQuality int, format string) int {
    switch format {
    case "webp":  return jpegQuality - 5   // WebP 80 ≈ JPEG 85
    case "avif":  return jpegQuality - 25  // AVIF 60 ≈ JPEG 85
    default:      return jpegQuality
    }
}
```

---

<a name="pipeline"></a>
## 10. Pipeline d'optimisation complet

Un pipeline d'optimisation production combine toutes ces techniques :

```
Image originale (JPEG/PNG 4-8K, 5-20 MB)
      │
      ▼
① Validation (magic bytes, dimensions max, taille max)
      │
      ▼
② Lecture EXIF (GPS → supprimer, orientation → appliquer)
      │
      ▼
③ Decode (image.Decode)
      │
      ▼
④ Resize (BiLinear → max 1920×1080, ratio préservé)
      │
      ▼
⑤ Watermark (position + couleur adaptative)
      │
      ▼
⑥ Encode selon le format demandé
      │
      ├── AVIF (qualité 60, speed 6)      → ~250 KB
      ├── WebP (qualité 80, lossy)        → ~340 KB
      └── JPEG (qualité 85, progressive)  → ~500 KB
      │
      ▼
⑦ Stocker dans Redis (clé = hash(image+wm+format))
      │
      ▼
⑧ Répondre avec Content-Type et Content-Encoding: gzip
```

---

<a name="libjpeg"></a>
## 11. libjpeg-turbo — JPEG rapide via cgo

**libjpeg-turbo** est une implémentation de libjpeg optimisée avec des instructions SIMD (SSE2, AVX2, NEON). Elle est 2-6x plus rapide que la bibliothèque standard JPEG de Go.

### Pourquoi cgo ?

La stdlib `image/jpeg` de Go est pure Go — pas d'accès SIMD. libjpeg-turbo est écrite en C avec des optimisations assembleur pour chaque architecture CPU.

```go
// import "github.com/pixiv/go-libjpeg/jpeg"  (wrapper cgo de libjpeg-turbo)

// Décodage ~3x plus rapide que stdlib
img, err := jpeg.Decode(file, &jpeg.DecoderOptions{})

// Encodage ~2x plus rapide que stdlib + support progressif
err = jpeg.Encode(output, img, &jpeg.EncoderOptions{
    Quality:     85,
    Progressive: true,  // JPEG progressif !
})
```

### Inconvénients de cgo

```
Avantages cgo        Inconvénients cgo
──────────────────   ──────────────────────────────
Performances SIMD    Compilation croisée compliquée
Bibliothèques C      Builds plus lents
Accès hardware       Dépendance libjpeg-turbo installée
                     Débogage plus complexe
                     Pas de compilation statique pure
```

**En Docker, le problème de dépendance est résolu :**

```dockerfile
FROM golang:1.23-alpine AS builder
RUN apk add --no-cache libjpeg-turbo-dev gcc musl-dev
COPY . .
RUN go build -o optimizer ./...

FROM alpine:3.20
RUN apk add --no-cache libjpeg-turbo
COPY --from=builder /app/optimizer .
```

---

<a name="watermark"></a>
## 12. Utilisation dans NWS Watermark

### Ce qui est fait ✅

```
✅ Resize BiLinear avec ratio préservé (max 1920×1080)
✅ Watermark position dynamique (4 coins)
✅ Couleur adaptative (luminance zone d'échantillonnage)
✅ Encodage JPEG qualité 85
✅ sync.Pool pour les buffers d'encodage
✅ Worker pool (1 slot par CPU)
```

### Ce qui manque ❌

```
❌ WebP/AVIF encoding (négociation Accept)
❌ Lazy decoding (DecodeConfig avant decode complet)
❌ Strip EXIF (sécurité + taille)
❌ Progressive JPEG
❌ Qualité adaptative selon les dimensions
❌ Parallélisation sampleLuminance
❌ libjpeg-turbo (2-6x plus rapide)
```

### Priorisation

```
Impact fort, effort faible :
  1. Strip EXIF               → -5 à 15% de taille, sécurité GPS
  2. Lazy decoding            → valider sans décoder les pixels
  3. Qualité adaptative       → miniatures moins lourdes

Impact fort, effort moyen :
  4. WebP encoding            → -30% de taille pour 97% des navigateurs
  5. Parallélisation sampling → watermark ~2x plus rapide

Impact fort, effort élevé :
  6. AVIF encoding            → -50% de taille
  7. libjpeg-turbo via cgo    → 2-6x plus rapide en encode/decode
```

---

<a name="résumé"></a>
## 13. Résumé

### Les formats en un coup d'œil

```
Photo 1920×1080 :
  JPEG  500 KB  → universel, toujours en fallback
  WebP  340 KB  → -32%, 97% support navigateur → à implémenter en priorité
  AVIF  250 KB  → -50%, 90% support            → si JPEG XL est trop loin
```

### Les optimisations par ordre d'impact

| Optimisation | Gain | Difficulté |
|---|---|---|
| Strip EXIF | -5 à 15% taille | ⭐ |
| WebP encoding | -30% taille | ⭐⭐ |
| Lazy decoding | -100% decode inutile | ⭐⭐ |
| AVIF encoding | -50% taille | ⭐⭐⭐ |
| Parallélisation pixels | -50% latence watermark | ⭐⭐⭐ |
| libjpeg-turbo (cgo) | 2-6x decode/encode | ⭐⭐⭐⭐ |
| Progressive JPEG | -20% taille + UX | ⭐⭐⭐⭐ |

### Règles à retenir

1. **Négocier le format** — AVIF > WebP > JPEG selon `Accept`
2. **Redimensionner avant d'encoder** — ne jamais envoyer 4K si on affiche en 1080p
3. **Strip EXIF** — sécurité (GPS) + gain de taille
4. **BiLinear pour le temps réel** — Lanczos pour l'export qualité
5. **Mesurer avant de paralléliser** — l'overhead goroutines peut annuler le gain
