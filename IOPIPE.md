# Cours : io.Pipe
## Connecter un Writer à un Reader sans buffer intermédiaire

---

## 📋 Table des matières

1. [C'est quoi io.Pipe ?](#intro)
2. [TCP ou en mémoire ?](#tcp)
3. [Le problème sans io.Pipe](#probleme)
4. [Les interfaces io.Reader et io.Writer](#interfaces)
5. [La structure interne](#structure)
6. [Le protocole ping-pong — wrCh et rdCh](#protocole)
7. [write() — comment le Writer bloque](#write)
8. [read() — comment le Reader débloque](#read)
9. [La fermeture — done, once, onceError](#fermeture)
10. [Propagation d'erreurs croisées](#erreurs)
11. [Pourquoi io.Pipe exige une goroutine](#goroutine)
12. [Utilisation dans NWS Watermark](#watermark)
13. [Cas d'usage classiques](#usages)
14. [Résumé](#résumé)

---

<a name="intro"></a>
## 1. C'est quoi io.Pipe ?

`io.Pipe` est un **tuyau synchrone en mémoire** qui connecte un côté qui **écrit** (`PipeWriter`) à un côté qui **lit** (`PipeReader`).

**Analogie :** C'est comme un tuyau de plomberie.
- Le **Writer** est le robinet — il envoie de l'eau
- Le **Reader** est la sortie — il reçoit l'eau
- **Pas de réservoir** — l'eau passe directement, sans s'accumuler

```
PipeWriter                   PipeReader
(robinet)                    (sortie)
    │                            │
    │──── données ──────────────►│
    │                            │
    │◄─── "j'ai lu N octets" ────│
    │
(bloqué jusqu'à confirmation)
```

`io.Pipe` est fourni par la bibliothèque standard Go dans le package `io`. Pas besoin d'installer quoi que ce soit.

```go
pr, pw := io.Pipe()
// pr = *PipeReader  (côté lecture)
// pw = *PipeWriter  (côté écriture)
```

---

<a name="tcp"></a>
## 2. TCP ou en mémoire ?

**Ni TCP, ni aucun protocole réseau.** `io.Pipe` est purement **en mémoire**, à l'intérieur d'un seul et même processus Go.

```
PipeWriter ──── channels Go ──── PipeReader
  (RAM)              (RAM)           (RAM)

Pas de socket. Pas de TCP. Pas de syscall réseau. Pas de kernel.
```

Le transport sous-jacent, ce sont **deux channels Go non-bufferisés** :

```go
wrCh chan []byte  // le writer dépose ses données ici
rdCh chan int     // le reader confirme combien il en a pris
```

Un channel Go est une structure en mémoire gérée par le runtime Go — la synchronisation se fait directement entre goroutines, sans jamais passer par le réseau.

---

### Comparaison avec les vrais "tuyaux"

| | `io.Pipe` (Go) | Pipe Unix ( \| ) | TCP |
|---|---|---|---|
| Emplacement | RAM (même processus) | Kernel (entre processus) | Réseau (entre machines) |
| Mécanisme | Channels Go | Syscalls `pipe(2)` | Sockets + protocole |
| Latence | ~ns | ~µs | ~ms |
| Nb de processus | 1 seul | 2+ | 2+ |
| Copies mémoire | 1 (`copy()`) | 2 (user→kernel→user) | 4+ (user→kernel→NIC→…) |

`io.Pipe` est le plus rapide des trois : zéro appel système, zéro réseau, une seule copie mémoire.

---

### Analogie corrigée

L'image du "tuyau de plomberie" suggère un réseau. En réalité c'est plus proche de **deux personnes dans la même pièce qui se passent des feuilles de papier à la main** :
- Pas de courrier (TCP)
- Pas de bureau de poste (kernel)
- Juste un échange direct entre deux goroutines du même programme

```
Goroutine A (writer)                Goroutine B (reader)
       │                                    │
       │ ──── "tiens, voilà les données" ──►│
       │ ◄─── "j'en ai pris N octets" ──── │
       │                                    │
     (bloquée jusqu'à confirmation)       (bloquée jusqu'à réception)
```

**Et TCP dans tout ça ?** TCP intervient **après** `io.Pipe`, quand `http.Post` envoie ce que le `PipeReader` a lu vers le réseau. `io.Pipe` ne fait que connecter le writer multipart au body HTTP — c'est HTTP/TCP qui transporte ensuite vers l'optimizer.

```
multipart.Writer → PipeWriter ══ PipeReader → http.Post → TCP → Optimizer
                   [en mémoire, même processus]   [réseau]
```

---

<a name="probleme"></a>
## 3. Le problème sans io.Pipe

### Scénario : envoyer un fichier multipart en HTTP

Tu veux construire un formulaire multipart (comme un upload de fichier) et l'envoyer directement à un serveur HTTP.

Le problème : `multipart.Writer` écrit dans un `io.Writer`, mais `http.Post` attend un `io.Reader`.

**Ce sont deux interfaces incompatibles.**

---

### Solution naïve — tout mettre en mémoire

```go
// ❌ Approche naïve : buffer intermédiaire
var buf bytes.Buffer                      // buffer en RAM
mw := multipart.NewWriter(&buf)           // écrit dans le buffer

part, _ := mw.CreateFormFile("image", "photo.jpg")
io.Copy(part, fichier)                    // copie le fichier dans le buffer
mw.Close()

// Maintenant le buffer contient TOUT le fichier en RAM
http.Post(url, mw.FormDataContentType(), &buf)
```

**Problèmes :**
- Pour un fichier de 50 MB → 50 MB alloués en RAM avant d'envoyer quoi que ce soit
- Pour 100 requêtes simultanées → 5 GB de RAM potentiellement alloués
- Le GC doit ensuite nettoyer tous ces buffers → pression mémoire

---

### Solution avec io.Pipe — zéro buffer

```go
// ✅ Avec io.Pipe : pas de buffer intermédiaire
pr, pw := io.Pipe()
mw := multipart.NewWriter(pw)

go func() {                               // goroutine séparée pour écrire
    part, _ := mw.CreateFormFile("image", "photo.jpg")
    io.Copy(part, fichier)                // écrit dans pw
    mw.Close()
    pw.Close()                            // signale la fin
}()

// http.Post lit depuis pr au fur et à mesure que la goroutine écrit
http.Post(url, mw.FormDataContentType(), pr)
```

**Avantages :**
- Zéro allocation intermédiaire — les données passent directement de l'un à l'autre
- Le réseau reçoit les données au fur et à mesure qu'elles sont construites
- La mémoire utilisée = taille d'un seul chunk, pas de l'image entière

| | Sans io.Pipe | Avec io.Pipe |
|---|---|---|
| Mémoire | Taille fichier complète | Taille d'un chunk (~32 KB) |
| Latence première donnée envoyée | Après construction complète | Immédiate |
| Pression GC | Forte (gros buffers à nettoyer) | Faible |
| Complexité | Simple | Nécessite une goroutine |

---

<a name="interfaces"></a>
## 3. Les interfaces io.Reader et io.Writer

Avant de comprendre io.Pipe, il faut comprendre ces deux interfaces fondamentales de Go.

### io.Writer

```go
type Writer interface {
    Write(p []byte) (n int, err error)
}
```

Contrat : "donne-moi un slice d'octets, je les consomme et te dis combien j'en ai pris."

Implémenté par : `os.File`, `bytes.Buffer`, `http.ResponseWriter`, `PipeWriter`, `multipart.Writer`...

### io.Reader

```go
type Reader interface {
    Read(p []byte) (n int, err error)
}
```

Contrat : "donne-moi un buffer vide, je le remplis et te dis combien d'octets j'y ai mis."

Implémenté par : `os.File`, `bytes.Buffer`, `http.Request.Body`, `PipeReader`, `strings.Reader`...

### Le problème de compatibilité

```
multipart.Writer  →  écrit dans  →  io.Writer
http.Post         →  lit depuis  →  io.Reader
```

Ces deux interfaces ne sont **pas directement compatibles**. `io.Pipe` est le pont entre les deux.

```
multipart.Writer ──► PipeWriter ══════ PipeReader ──► http.Post
                       (io.Writer)      (io.Reader)
```

---

<a name="structure"></a>
## 4. La structure interne

Voici l'intégralité de la structure `pipe` qui est au cœur du mécanisme :

```go
type pipe struct {
    wrMu sync.Mutex  // empêche deux Write() simultanés
    wrCh chan []byte  // le writer envoie son slice ici
    rdCh chan int     // le reader répond combien d'octets il a consommés

    once sync.Once    // garantit que done est fermé une seule fois
    done chan struct{} // canal de signal : fermé = pipe terminé
    rerr onceError    // erreur côté lecture (stockée une seule fois)
    werr onceError    // erreur côté écriture
}
```

### Deux channels, pas de buffer

Le choix clé de la conception : **utiliser des channels Go** plutôt qu'un `[]byte` partagé.

Pourquoi ? Parce que les channels Go sont synchronisants par nature.

```
wrCh chan []byte  →  unbuffered : le writer BLOQUE jusqu'à ce que le reader reçoive
rdCh chan int     →  unbuffered : le reader BLOQUE jusqu'à ce que le writer reçoive la confirmation
```

Un channel unbuffered (`make(chan T)`) n'a pas de file d'attente interne.
L'envoi bloque tant que personne ne reçoit. La réception bloque tant que personne n'envoie.
C'est exactement ce qu'on veut : **synchronisation directe writer ↔ reader**.

### La construction

```go
func Pipe() (*PipeReader, *PipeWriter) {
    pw := &PipeWriter{r: PipeReader{pipe: pipe{
        wrCh: make(chan []byte),   // unbuffered
        rdCh: make(chan int),      // unbuffered
        done: make(chan struct{}), // signal de fermeture
    }}}
    return &pw.r, pw
}
```

`PipeWriter` **contient** un `PipeReader` (champ `r`). Les deux partagent la même structure `pipe`.
C'est pourquoi `pw.r.pipe` et la pipe interne du reader sont identiques — c'est le même objet.

---

<a name="protocole"></a>
## 5. Le protocole ping-pong — wrCh et rdCh

Le transfert de données suit un protocole en deux temps :

```
Writer                              Reader
  │                                   │
  │  p.wrCh <- b  ──────────────────► │  bw := <-p.wrCh
  │  (envoie le slice entier)         │  (reçoit la référence au slice)
  │                                   │
  │                                   │  nr := copy(dst, bw)
  │                                   │  (copie ce qu'il peut dans son buffer)
  │                                   │
  │  nw := <-p.rdCh  ◄────────────── │  p.rdCh <- nr
  │  (reçoit : "j'ai lu N octets")   │  (confirme combien il a consommé)
  │                                   │
  │  b = b[nw:]                       │
  │  (avance dans le slice)           │
  │  (recommence si reste des données)│
```

### Pourquoi le writer envoie le slice entier ?

Le writer envoie une **référence** au slice (pas une copie). Le reader copie ce dont il a besoin avec `copy()`.

```go
// Dans read()
bw := <-p.wrCh        // bw pointe sur le même tableau que b dans write()
nr := copy(b, bw)     // copie depuis bw vers le buffer du reader
p.rdCh <- nr          // confirme
```

```go
// Dans write()
case p.wrCh <- b:     // envoie la référence
    nw := <-p.rdCh    // attend : combien a été consommé ?
    b = b[nw:]        // avance dans le slice original
    n += nw
```

### Exemple concret avec des tailles différentes

Le reader peut avoir un petit buffer (4 KB), le writer peut envoyer 100 KB. La boucle dans `write()` gère ça :

```
Iteration 1 :  writer envoie b[0:100000]
               reader copie 4096 octets dans son buffer
               reader répond : 4096
               writer avance : b = b[4096:]

Iteration 2 :  writer envoie b[4096:100000]
               reader copie 4096 octets
               reader répond : 4096
               writer avance : b = b[8192:]

...            (continue jusqu'à b vide)

Iteration N :  writer envoie b[98304:100000]  (1696 octets restants)
               reader copie 1696 octets
               reader répond : 1696
               writer avance : b = b[100000:] (vide)
               → boucle terminée, write() retourne
```

---

<a name="write"></a>
## 6. write() — comment le Writer bloque

```go
func (p *pipe) write(b []byte) (n int, err error) {
    // Étape 1 : vérifier si le pipe est déjà fermé
    select {
    case <-p.done:
        return 0, p.writeCloseError()
    default:
        p.wrMu.Lock()        // verrou : un seul writer à la fois
        defer p.wrMu.Unlock()
    }

    // Étape 2 : envoyer les données par chunks jusqu'à épuisement
    for once := true; once || len(b) > 0; once = false {
        select {
        case p.wrCh <- b:        // envoie le slice au reader → bloque ici
            nw := <-p.rdCh       // attend la confirmation → bloque ici
            b = b[nw:]           // avance dans le slice
            n += nw
        case <-p.done:           // pipe fermé pendant l'attente
            return n, p.writeCloseError()
        }
    }
    return n, nil
}
```

### La boucle "do/while" de Go

```go
for once := true; once || len(b) > 0; once = false {
```

Go n'a pas de `do/while`. Cette syntaxe est la façon idiomatique de l'écrire :
- `once := true` → condition initiale vraie → la boucle tourne au moins une fois
- Après la première itération : `once = false`, et on continue si `len(b) > 0`

**Pourquoi exécuter au moins une fois ?** Pour gérer le cas d'un `Write([]byte{})` (slice vide) — il faut quand même contacter le reader pour ne pas perdre la synchronisation.

### Le select avec deux cases

```go
select {
case p.wrCh <- b:    // cas 1 : le reader est prêt → transfert
    ...
case <-p.done:       // cas 2 : pipe fermé → erreur
    ...
}
```

Le `select` attend **le premier case qui se débloque**. Si le reader tarde, le writer reste bloqué sur `p.wrCh <- b`. Dès que le reader fait `<-p.wrCh`, les deux se débloquent simultanément.

### Le mutex wrMu

`wrMu` empêche deux goroutines d'appeler `Write()` en même temps. Sans lui, deux writers pourraient entrelever leurs chunks dans `wrCh`, corrompant le flux.

```go
// Sans mutex — problème potentiel :
Goroutine A : p.wrCh <- "Hello"
Goroutine B : p.wrCh <- "World"
// Le reader pourrait recevoir "World" avant "Hello" ou un mélange des deux
```

---

<a name="read"></a>
## 7. read() — comment le Reader débloque

```go
func (p *pipe) read(b []byte) (n int, err error) {
    // Étape 1 : vérification rapide sans bloquer
    select {
    case <-p.done:
        return 0, p.readCloseError()
    default:
    }

    // Étape 2 : attendre des données ou une fermeture
    select {
    case bw := <-p.wrCh:    // reçoit le slice du writer
        nr := copy(b, bw)   // copie dans le buffer du caller
        p.rdCh <- nr        // confirme au writer combien a été consommé
        return nr, nil
    case <-p.done:
        return 0, p.readCloseError()
    }
}
```

### Pourquoi deux select ?

**Premier select (avec `default`) :** vérification non-bloquante.
Si le pipe est déjà fermé, on retourne immédiatement sans entrer dans l'attente.

```go
select {
case <-p.done:     // fermé ? → erreur immédiate
    return 0, ...
default:           // pas fermé → on continue (ne bloque pas)
}
```

Sans ce premier select, les deux cases du second select seraient équivalents mais on perdrait la garantie de retour immédiat si done est déjà fermé au moment de l'appel.

**Second select :** attente bloquante sur deux évènements possibles.
```go
select {
case bw := <-p.wrCh:  // données disponibles → on les consomme
case <-p.done:         // pipe fermé pendant l'attente → erreur
}
```

### copy() — une seule copie mémoire

```go
nr := copy(b, bw)
```

`copy` copie directement depuis le slice du writer (`bw`) vers le buffer du caller (`b`).
C'est la **seule et unique copie** de la donnée dans tout le pipeline.
Pas de buffer intermédiaire, pas d'allocation.

---

<a name="fermeture"></a>
## 8. La fermeture — done, once, onceError

### Le canal done

`done chan struct{}` est un **canal de signal**.
Fermer un canal (avec `close()`) en Go débloque immédiatement **tous** les lecteurs en attente.

```go
// Tous les select qui attendent sur done se débloquent instantanément
case <-p.done:
    return 0, p.readCloseError()
```

C'est le mécanisme standard en Go pour broadcaster un signal à plusieurs goroutines simultanément.

### sync.Once — fermer une seule fois

Appeler `close()` deux fois sur le même canal provoque une **panique** en Go.
`sync.Once` garantit qu'une fonction n'est exécutée qu'une seule fois, peu importe combien de fois elle est appelée.

```go
p.once sync.Once

// Peu importe si closeRead ET closeWrite sont appelés :
// close(p.done) ne sera exécuté qu'une seule fois
p.once.Do(func() { close(p.done) })
```

```go
func (p *pipe) closeRead(err error) error {
    if err == nil { err = ErrClosedPipe }
    p.rerr.Store(err)
    p.once.Do(func() { close(p.done) })  // ferme done UNE SEULE FOIS
    return nil
}

func (p *pipe) closeWrite(err error) error {
    if err == nil { err = EOF }
    p.werr.Store(err)
    p.once.Do(func() { close(p.done) })  // si déjà fermé, Do ne fait rien
    return nil
}
```

### onceError — stocker la première erreur

```go
type onceError struct {
    sync.Mutex
    err error
}

func (a *onceError) Store(err error) {
    a.Lock()
    defer a.Unlock()
    if a.err != nil {
        return    // déjà une erreur stockée → on ignore la nouvelle
    }
    a.err = err
}

func (a *onceError) Load() error {
    a.Lock()
    defer a.Unlock()
    return a.err
}
```

**Principe :** la **première erreur gagne**.
Si tu appelles `CloseWithError(err1)` puis `CloseWithError(err2)`, seule `err1` est conservée.
C'est important : l'erreur originelle est souvent la plus informative.

---

<a name="erreurs"></a>
## 9. Propagation d'erreurs croisées

L'une des fonctionnalités les plus subtiles de `io.Pipe` : les erreurs se propagent **dans les deux sens**.

### readCloseError

```go
func (p *pipe) readCloseError() error {
    rerr := p.rerr.Load()
    if werr := p.werr.Load(); rerr == nil && werr != nil {
        return werr   // le writer a fermé avec une erreur → le reader la voit
    }
    return ErrClosedPipe
}
```

**Logique :** Si le reader est fermé (`rerr == nil` = pas d'erreur côté reader), mais que le writer a fermé avec une erreur, le reader retourne l'erreur du writer.

```
pw.CloseWithError(fmt.Errorf("erreur multipart"))
          ↓
pr.Read() retourne fmt.Errorf("erreur multipart")
```

### writeCloseError

```go
func (p *pipe) writeCloseError() error {
    werr := p.werr.Load()
    if rerr := p.rerr.Load(); werr == nil && rerr != nil {
        return rerr   // le reader a fermé avec une erreur → le writer la voit
    }
    return ErrClosedPipe
}
```

**Logique symétrique :** Si le reader ferme avec une erreur (ex: `http.Request.Body` annulé), le writer reçoit cette erreur à son prochain `Write()`.

### Cas d'usage concret

```go
go func() {
    _, err := io.Copy(part, bigFile)
    if err != nil {
        pw.CloseWithError(err)  // ← l'erreur de lecture fichier
        return
    }
    pw.Close()
}()

_, err := http.Post(url, contentType, pr)
// Si la goroutine ci-dessus a CloseWithError → http.Post reçoit cette erreur
```

### Tableau des cas

| Qui ferme | Comment | Ce que voit l'autre côté |
|---|---|---|
| `pw.Close()` | `werr = EOF` | `pr.Read()` retourne `(0, io.EOF)` |
| `pw.CloseWithError(err)` | `werr = err` | `pr.Read()` retourne `(0, err)` |
| `pr.Close()` | `rerr = ErrClosedPipe` | `pw.Write()` retourne `(0, ErrClosedPipe)` |
| `pr.CloseWithError(err)` | `rerr = err` | `pw.Write()` retourne `(0, err)` |

---

<a name="goroutine"></a>
## 10. Pourquoi io.Pipe exige une goroutine

`io.Pipe` est **synchrone et bloquant**. Le writer bloque jusqu'à ce que le reader consomme, et vice versa.

Si tu utilises le writer et le reader dans la **même goroutine** → **deadlock** immédiat.

```go
// ❌ DEADLOCK — ne jamais faire ça
pr, pw := io.Pipe()

pw.Write([]byte("hello"))  // bloque ici indéfiniment
                           // → attend que quelqu'un lise depuis pr
                           // → mais pr est dans la même goroutine
                           // → personne ne lit → blocage infini
pr.Read(buf)               // jamais atteint
```

**La règle :** writer et reader doivent toujours être dans des **goroutines séparées**.

```go
// ✅ Correct
pr, pw := io.Pipe()

go func() {
    pw.Write([]byte("hello"))  // goroutine A : écrit
    pw.Close()
}()

buf := make([]byte, 1024)
pr.Read(buf)  // goroutine principale : lit
```

### Pourquoi cette contrainte est une feature

Le deadlock potentiel force à structurer le code proprement : **producteur et consommateur sont clairement séparés**.
C'est exactement le modèle concurrent Go : "communicate by sharing, don't share by communicating".

---

<a name="watermark"></a>
## 11. Utilisation dans NWS Watermark

Dans `api/main.go`, `io.Pipe` est utilisé pour streamer une image vers l'optimizer sans la charger entièrement en RAM une seconde fois.

```go
func sendToOptimizer(optimizerURL, filename string, data []byte, wmText, wmPosition string) ([]byte, error) {
    pr, pw := io.Pipe()
    mw := multipart.NewWriter(pw)

    // Goroutine productrice : construit le formulaire multipart et écrit dans pw
    go func() {
        part, err := mw.CreateFormFile("image", filename)
        if err != nil {
            pw.CloseWithError(err)  // propagé vers http.Post via pr
            return
        }
        io.Copy(part, bytes.NewReader(data))
        mw.WriteField("wm_text", wmText)
        mw.WriteField("wm_position", wmPosition)
        mw.Close()
        pw.Close()  // signale EOF → http.Post sait que le body est terminé
    }()

    // http.Post lit depuis pr au fur et à mesure que la goroutine écrit dans pw
    resp, err := httpClient.Post(optimizerURL+"/optimize", mw.FormDataContentType(), pr)
    if err != nil {
        return nil, err
    }
    defer resp.Body.Close()
    return io.ReadAll(resp.Body)
}
```

### Flux de données

```
bytes.NewReader(data)
        │
        │ io.Copy
        ▼
multipart.Writer (mw)
        │
        │ Write() → pw (PipeWriter)
        ▼
    [io.Pipe]  ←── synchronisation ici (pas de buffer)
        │
        │ Read() ← pr (PipeReader)
        ▼
http.Post body reader
        │
        │ envoi réseau
        ▼
   Optimizer /optimize
```

### Pourquoi pas juste `bytes.NewReader(data)` directement ?

`data` contient déjà l'image brute en mémoire. On pourrait l'envoyer directement.
Mais on doit d'abord **l'emballer dans un formulaire multipart** avec les champs `wm_text` et `wm_position`.
`multipart.Writer` produit un `io.Writer`, pas un `io.Reader`. `io.Pipe` résout cette incompatibilité.

---

### Flow complet — du front à l'optimizer

C'est important de bien situer où s'arrête chaque protocole :

```
Front (navigateur)
    │
    │  HTTP/TCP  POST /upload  (réseau)
    │  body = image brute en multipart/form-data
    ▼
API (port 3000) — même processus Go
    │
    │  r.FormFile("image") → lit le body HTTP → data []byte en RAM
    │
    │  ┌─────────────────── EN MÉMOIRE (pas de réseau) ───────────────────┐
    │  │                                                                   │
    │  │  Goroutine ──► multipart.Writer ──► PipeWriter ══ PipeReader     │
    │  │                (reconstruit le      (io.Pipe en RAM,             │
    │  │                 formulaire avec      même processus)             │
    │  │                 wm_text, wm_position)                            │
    │  │                                                                   │
    │  └───────────────────────────────────────────────────────────────────┘
    │
    │  http.Post lit depuis PipeReader → envoie sur TCP (réseau)
    ▼
Optimizer (port 3001)
    │
    │  r.FormFile("image") → décode → resize → watermark → JPEG encode
    │
    │  réponse HTTP/TCP → retour vers l'API (réseau)
    ▼
API
    │  Redis.Set(cacheKey, résultat)
    │  répond 200 + image au front (HTTP/TCP)
    ▼
Front
```

**Résumé des protocoles :**

| Segment | Protocole | Où |
|---|---|---|
| Front → API | HTTP/TCP | Réseau |
| API interne (data → multipart) | `io.Pipe` (channels Go) | RAM, même processus |
| API → Optimizer | HTTP/TCP | Réseau |
| Optimizer → API | HTTP/TCP | Réseau |
| API → Front | HTTP/TCP | Réseau |

**Le pipe ne traverse jamais le réseau.** Il est uniquement là pour brancher le `multipart.Writer` (qui produit un `io.Writer`) sur le body de `http.Post` (qui attend un `io.Reader`), le tout en RAM dans le processus de l'API.

---

<a name="usages"></a>
## 12. Cas d'usage classiques

### 1. Upload multipart vers un service tiers (notre cas)

```go
pr, pw := io.Pipe()
mw := multipart.NewWriter(pw)
go func() {
    part, _ := mw.CreateFormFile("file", "data.csv")
    io.Copy(part, csvReader)
    mw.Close(); pw.Close()
}()
http.Post("https://api.example.com/upload", mw.FormDataContentType(), pr)
```

### 2. Compression à la volée

Compresser un fichier et l'uploader sans créer de fichier temporaire compressé :

```go
pr, pw := io.Pipe()
go func() {
    gz := gzip.NewWriter(pw)
    io.Copy(gz, fichierSource)
    gz.Close()
    pw.Close()
}()
http.Post(url, "application/gzip", pr)
```

### 3. Transformation de données en streaming

Convertir du JSON en CSV et l'écrire directement dans une réponse HTTP :

```go
pr, pw := io.Pipe()
go func() {
    csv := csv.NewWriter(pw)
    for _, row := range jsonData {
        csv.Write(convertToRow(row))
    }
    csv.Flush()
    pw.Close()
}()
w.Header().Set("Content-Type", "text/csv")
io.Copy(w, pr)  // écrit directement dans la réponse HTTP
```

### 4. Test d'une fonction qui attend un io.Reader

```go
pr, pw := io.Pipe()
go func() {
    pw.Write([]byte(`{"name": "test"}`))
    pw.Close()
}()
maFonction(pr)  // maFonction attend un io.Reader
```

---

<a name="résumé"></a>
## 13. Résumé

### Ce que fait io.Pipe

```
Problème : multipart.Writer (io.Writer) ←→ http.Post (io.Reader)
Solution : io.Pipe connecte les deux sans buffer intermédiaire
```

### Les pièces du mécanisme

| Élément | Rôle |
|---|---|
| `wrCh chan []byte` | Le writer envoie son slice ici, bloque jusqu'à réception |
| `rdCh chan int` | Le reader confirme combien il a consommé |
| `done chan struct{}` | Signal de fermeture broadcasté à toutes les goroutines |
| `sync.Once` | Garantit que `close(done)` n'est appelé qu'une seule fois |
| `onceError` | Stocke la première erreur (writer ou reader) |
| `wrMu sync.Mutex` | Empêche deux Write() simultanés d'entremêler leurs données |

### Les règles à retenir

1. **Toujours une goroutine** — writer et reader ne peuvent pas être dans la même goroutine (deadlock)
2. **Toujours fermer** — appeler `pw.Close()` en fin de goroutine pour signaler EOF au reader
3. **Propager les erreurs** — utiliser `pw.CloseWithError(err)` si quelque chose échoue côté writer
4. **Zéro buffer** — les données passent directement, pas d'allocation intermédiaire

### Comparaison rapide

| | `bytes.Buffer` | `io.Pipe` |
|---|---|---|
| Buffer en mémoire | Oui (taille du contenu) | Non |
| Goroutine requise | Non | Oui |
| Latence | Après construction complète | Immédiate |
| Usage typique | Accumulation simple | Streaming writer→reader |
| Thread-safe | Non | Oui |
