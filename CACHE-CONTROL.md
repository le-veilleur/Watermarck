# Cache-Control — `net/http`

`Cache-Control` est un header HTTP qui dit **à qui** une réponse peut être mise en cache, **combien de temps**, et **dans quelles conditions** elle est encore valide. Bien utilisé, il élimine des aller-retours réseau entiers. Mal utilisé, il sert des données périmées ou force des rechargements inutiles.

**Fichier principal** : `api/main.go`
**Package Go** : `net/http` (stdlib — aucune dépendance externe)

---

## Anatomie du header

```
Cache-Control: public, max-age=3600, must-revalidate
               ──┬───  ──────┬─────  ──────────┬─────
                 │           │                  │
                 │      durée en secondes       │
                 │                              └── comportement à expiration
                 └── qui peut cacher (navigateur seul, ou CDN aussi)
```

Un header `Cache-Control` est une liste de **directives** séparées par des virgules. On peut en mettre plusieurs — elles se combinent.

---

## Directives essentielles

### Contrôle du stockage

| Directive | Signification |
|---|---|
| `no-store` | Ne **jamais** stocker cette réponse — ni navigateur, ni CDN. Chaque requête repart au serveur. |
| `no-cache` | Stocker est autorisé, mais **re-valider** auprès du serveur avant de servir. |
| `private` | Seul le navigateur de l'utilisateur peut cacher — pas les CDN ni les proxies intermédiaires. |
| `public` | N'importe quel intermédiaire (CDN, proxy) peut cacher cette réponse. |

**Pourquoi `no-store` ≠ `no-cache`** : `no-cache` stocke localement mais interroge le serveur à chaque fois (le serveur peut répondre 304 Not Modified sans renvoyer le body). `no-store` interdit même le stockage — rien n'est jamais écrit sur disque ni en mémoire côté client.

---

### Durée de vie

| Directive | Signification |
|---|---|
| `max-age=N` | Valide **N secondes** depuis la réception. Cible : navigateur + CDN si `public`. |
| `s-maxage=N` | Comme `max-age` mais **uniquement pour les CDN/proxies partagés** — override `max-age` pour eux. |
| `immutable` | Garantit que la ressource ne changera **jamais** pendant sa durée de vie. Le navigateur ne re-valide même pas sur F5. |

```go
// Image statique dont l'URL change si le contenu change (content hash)
// → le navigateur peut la garder 1 an sans jamais redemander
w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")

// Réponse CDN différente du navigateur : CDN cache 1h, navigateur 5 min
w.Header().Set("Cache-Control", "public, max-age=300, s-maxage=3600")
```

---

### Comportement à expiration

| Directive | Signification |
|---|---|
| `must-revalidate` | Une fois expiré, **interdire** de servir une copie périmée, même si le serveur est injoignable. |
| `proxy-revalidate` | Comme `must-revalidate` mais pour les CDN uniquement. |
| `stale-while-revalidate=N` | Servir la copie périmée **pendant N secondes** pendant qu'une re-validation se fait en arrière-plan. |
| `stale-if-error=N` | Si le serveur est en erreur (5xx), servir la copie périmée pendant N secondes maximum. |

```go
// API dont les données changent souvent, mais où une réponse légèrement périmée
// est acceptable pour ne pas bloquer l'utilisateur
w.Header().Set("Cache-Control", "public, max-age=60, stale-while-revalidate=30")
```

---

## Usage dans `net/http`

### Écrire un header Cache-Control (réponse)

```go
// Pas de cache du tout — données sensibles, résultats dynamiques
w.Header().Set("Cache-Control", "no-store")

// Re-valider à chaque fois, mais stocker pour les 304
w.Header().Set("Cache-Control", "no-cache, must-revalidate")

// Cache public 24h (assets statiques sans hash dans l'URL)
w.Header().Set("Cache-Control", "public, max-age=86400")

// Cache permanent (URL contient un hash du contenu)
w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
```

**Règle** : appeler `w.Header().Set(...)` **avant** `w.WriteHeader()` ou tout `w.Write()`. Une fois le status code écrit, les headers sont envoyés et ne peuvent plus être modifiés.

```go
// ✅ Correct
w.Header().Set("Cache-Control", "no-store")
w.Header().Set("Content-Type", "image/webp")
w.WriteHeader(http.StatusOK)
w.Write(data)

// ❌ Trop tard — les headers sont déjà partis
w.Write(data)
w.Header().Set("Cache-Control", "no-store")  // ignoré silencieusement
```

---

### Lire Cache-Control depuis la requête entrante

Le client peut aussi envoyer `Cache-Control` pour demander un comportement spécifique. C'est utilisé par les navigateurs (ex: Ctrl+F5 envoie `Cache-Control: no-cache`).

```go
func handleUpload(w http.ResponseWriter, r *http.Request) {
    cc := r.Header.Get("Cache-Control")

    // Ctrl+F5 / "hard refresh" — le navigateur demande une ressource fraîche
    if strings.Contains(cc, "no-cache") || strings.Contains(cc, "no-store") {
        // invalider le cache Redis avant de traiter
        invalidateCache(cacheKey)
    }
    // ...
}
```

**Directives courantes envoyées par les clients** :

| Directive client | Quand | Sens |
|---|---|---|
| `no-cache` | Ctrl+F5 (Chrome/Firefox) | "Re-valide avant de me servir quoi que ce soit" |
| `no-store` | Formulaires sensibles | "Ne cache rien du tout" |
| `max-age=0` | Refresh normal (F5) | "Considère tout comme expiré, re-valide" |
| `only-if-cached` | Offline / Service Worker | "Réponds seulement si tu as une copie fraîche" |

---

## Validation conditionnelle — ETag et `Last-Modified`

`Cache-Control` dit *combien de temps* cacher. L'ETag dit *si ça a changé* quand la durée est dépassée. Les deux se combinent.

### ETag (contenu)

L'ETag est une empreinte du contenu. Le navigateur la stocke et la renvoie au prochain hit expiré. Si le contenu n'a pas changé, le serveur répond **304 Not Modified** sans body → économie de bande passante.

```go
import (
    "crypto/sha256"
    "encoding/hex"
    "net/http"
)

func serveImage(w http.ResponseWriter, r *http.Request, data []byte) {
    // Calculer l'ETag à partir du contenu
    sum := sha256.Sum256(data)
    etag := `"` + hex.EncodeToString(sum[:8]) + `"`  // guillemets obligatoires (RFC 7232)

    w.Header().Set("ETag", etag)
    w.Header().Set("Cache-Control", "public, max-age=3600, must-revalidate")

    // Le navigateur renvoie l'ETag dans If-None-Match à la prochaine requête
    if r.Header.Get("If-None-Match") == etag {
        w.WriteHeader(http.StatusNotModified)  // 304 — pas de body, juste les headers
        return
    }

    w.Header().Set("Content-Type", detectContentType(data))
    w.Write(data)
}
```

**Flow complet** :

```
1ère requête               Serveur
GET /image/abc         →   200 OK
                           ETag: "a3f4c8d1"
                           Cache-Control: max-age=3600
                       ←   [body : 1.2 MB]

(1 heure plus tard — cache expiré)

2ème requête               Serveur
GET /image/abc         →   304 Not Modified    ← si contenu inchangé
If-None-Match: "a3f4c8d1"    (0 bytes de body)
                       ←
    ou
                           200 OK + nouveau body  ← si contenu changé
                           ETag: "b7e2a1f9"
```

### `Last-Modified` (date)

Alternative à l'ETag basée sur la date de modification. Moins précis (résolution à la seconde) mais utile pour les fichiers statiques.

```go
modTime := time.Now().UTC()
w.Header().Set("Last-Modified", modTime.Format(http.TimeFormat))

// Le navigateur renvoie If-Modified-Since à la prochaine requête
ifModSince := r.Header.Get("If-Modified-Since")
if ifModSince != "" {
    t, err := http.ParseTime(ifModSince)
    if err == nil && !modTime.After(t) {
        w.WriteHeader(http.StatusNotModified)
        return
    }
}
```

**`http.TimeFormat`** est la constante Go pour le format RFC 1123 attendu par HTTP :
`"Mon, 02 Jan 2006 15:04:05 GMT"`

### ETag vs Last-Modified

| | ETag | Last-Modified |
|---|---|---|
| Précision | Exacte (hash du contenu) | Seconde (peut rater des changements rapides) |
| Calcul | SHA-256 ou autre hash | Date de modification |
| Cas d'usage idéal | Contenu calculé dynamiquement | Fichiers statiques sur disque |
| Combinaison | Oui — ETag a priorité si les deux sont présents | Oui |

---

## Interaction avec `Vary`

Le header `Vary` est déjà utilisé dans ce projet (`api/main.go:94`) :

```go
w.Header().Set("Vary", "Accept")
```

**Pourquoi c'est important avec Cache-Control** : si un CDN cache une image en WebP pour `Accept: image/webp`, il ne doit pas la servir à un client qui n'envoie pas ce header (et qui attendrait du JPEG). `Vary: Accept` lui dit "crée une entrée de cache distincte par valeur du header `Accept`".

```
Requête A : Accept: image/webp  → cache["url + webp"] = WebP image
Requête B : Accept: image/jpeg  → cache["url + jpeg"] = JPEG image
                                   (deux entrées distinctes grâce à Vary)
```

Sans `Vary: Accept`, un CDN pourrait servir du WebP à un client qui demande du JPEG.

---

## Stratégie pour ce projet

### `POST /upload` — réponse du handler principal

```go
// Les images watermarkées sont uniques par (image + texte + position + format).
// POST n'est pas mis en cache par les navigateurs ni les CDN par défaut,
// mais on le rend explicite pour les proxies qui seraient mal configurés.
w.Header().Set("Cache-Control", "no-store")
```

**Pourquoi `no-store` et pas `public, max-age=...`** : le résultat dépend du contenu uploadé par l'utilisateur — il n'y a pas d'URL stable identifiant une ressource. Cacher ne fait aucun sens ici.

---

### Endpoint GET hypothétique — image watermarkée par hash

Si on ajoutait un endpoint `GET /image/{hash}` pour récupérer une image déjà traitée depuis Redis/MinIO :

```go
func serveWatermarked(w http.ResponseWriter, r *http.Request) {
    hash := r.PathValue("hash")  // Go 1.22+

    data, err := redis.Get(hash)
    if err != nil {
        http.NotFound(w, r)
        return
    }

    // Le hash identifie de façon unique le contenu :
    // si l'URL est valide, le contenu ne changera jamais → immutable
    etag := `"` + hash[:16] + `"`
    w.Header().Set("ETag", etag)
    w.Header().Set("Cache-Control", "public, max-age=604800, immutable")  // 7 jours
    w.Header().Set("Vary", "Accept")

    if r.Header.Get("If-None-Match") == etag {
        w.WriteHeader(http.StatusNotModified)
        return
    }

    ct := detectContentType(data)
    w.Header().Set("Content-Type", ct)
    w.Write(data)
}
```

**Pourquoi `immutable`** : le hash dans l'URL est calculé depuis `sha256(image + wmText + wmPosition + wmFormat)`. Si l'URL existe, le contenu est par définition immuable. `immutable` dit au navigateur "pas besoin de re-valider même sur F5 — tu peux faire confiance à ta copie locale".

---

### Headers de timing (déjà en place)

```go
w.Header().Set("X-T-Read", fmtMs(readDur))       // api/main.go:92
w.Header().Set("X-T-Optimizer", fmtMs(optimizerDur))  // api/main.go:93
```

Ces headers personnalisés ne sont pas affectés par Cache-Control, mais si une réponse est mise en cache, **ils ne seront pas mis à jour** lors d'un hit cache (le navigateur sert la réponse stockée, les headers inclus). À garder en tête pour le debug.

---

## Valider dans le navigateur

**Onglet Network de DevTools** :

| Colonne | Ce qu'elle montre |
|---|---|
| `Status` | `200` = depuis le serveur, `304` = revalidé (pas de body), `200 (from cache)` = servi localement |
| `Size` | `(disk cache)` ou `(memory cache)` indique un hit cache local |
| Response Headers | Vérifier `Cache-Control`, `ETag`, `Vary` |
| Request Headers | Vérifier `If-None-Match`, `If-Modified-Since`, `Cache-Control` (envoyé par le navigateur) |

**Tester les cas limites** :

```bash
# Simuler un client qui demande une ressource fraîche (comme Ctrl+F5)
curl -H "Cache-Control: no-cache" http://localhost:4000/upload ...

# Vérifier les headers de réponse
curl -I http://localhost:4000/upload ...

# Simuler un hit conditionnel avec ETag
curl -H 'If-None-Match: "a3f4c8d1"' http://localhost:4000/image/abc
# → 304 si le contenu n'a pas changé
```

---

## Décision rapide

```
La ressource est-elle privée à l'utilisateur ?
    Oui → private  (ne pas laisser les CDN cacher)
    Non ↓

Le contenu change-t-il à chaque requête ?
    Oui → no-store
    Non ↓

L'URL change-t-elle quand le contenu change ? (content hash)
    Oui → public, max-age=31536000, immutable
    Non ↓

Combien de temps le contenu est-il stable ?
    < 1 min   → no-cache, must-revalidate  (+ ETag pour les 304)
    ~ quelques heures → public, max-age=3600, must-revalidate
    ~ quelques jours  → public, max-age=86400, stale-while-revalidate=3600
```

---

## Points de vigilance

- **Ne jamais mettre `Cache-Control` après `w.Write()`** — les headers sont envoyés au premier write, la modification est silencieusement ignorée.
- **`no-cache` ne veut pas dire "pas de cache"** — ça veut dire "re-valide avant de servir". Pour interdire tout stockage, utiliser `no-store`.
- **`Vary: Accept` est indispensable** si on retourne différents formats selon le header `Accept` (WebP vs JPEG) et qu'un CDN est en jeu — sans lui, un CDN sert la mauvaise variante.
- **Les ETag sont entre guillemets** dans le header HTTP : `ETag: "abc123"` et non `ETag: abc123` (RFC 7232 — Go ne les ajoute pas automatiquement).
- **POST n'est jamais mis en cache par défaut**, mais un proxy mal configuré peut le faire — être explicite avec `no-store` sur les endpoints POST sensibles.
- **`immutable` n'est respecté que si `max-age` est aussi présent** — sans lui, `immutable` est ignoré par la plupart des navigateurs.
