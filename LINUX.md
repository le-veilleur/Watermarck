# Linux & OS — Sous le capot d'un serveur haute performance

## Table des matières

1. [Pourquoi comprendre l'OS ?](#pourquoi)
2. [Le problème C10K](#c10k)
3. [epoll — multiplexage d'I/O](#epoll)
4. [io_uring — I/O asynchrone sans syscall](#io_uring)
5. [Zero-copy — sendfile et splice](#zero-copy)
6. [mmap — fichiers en mémoire virtuelle](#mmap)
7. [Hiérarchie des caches CPU](#cpu-cache)
8. [NUMA — accès mémoire non-uniforme](#numa)
9. [Appels système (syscall) — le coût caché](#syscall)
10. [Processus vs Thread vs Goroutine](#goroutine)
11. [Docker FROM scratch — image minimale](#scratch)
12. [Primitives Linux dans notre projet](#projet)
13. [Outils de diagnostic](#outils)

---

<a name="pourquoi"></a>
## 1. Pourquoi comprendre l'OS ?

Un serveur Go tourne sur Linux. Quand tu appelles `http.ListenAndServe`, Go appelle l'OS, qui appelle le noyau, qui appelle le matériel.

```
Application Go
   │
   ▼ syscall (read/write/accept/epoll_wait...)
Noyau Linux (kernel)
   │
   ▼ pilotes (drivers)
Matériel (NIC, disque, RAM, CPU)
```

Comprendre ce qui se passe sous `net/http` explique :
- pourquoi un serveur plante à 1000 connexions simultanées
- pourquoi `sendfile` est 3x plus rapide que `read` + `write`
- pourquoi 8 goroutines ≠ 8 threads OS

---

<a name="c10k"></a>
## 2. Le problème C10K

**C10K = 10 000 connexions simultanées sur un seul serveur**

Posé en 1999 par Dan Kegel. À l'époque, un serveur Apache créait **1 thread par connexion**.

### L'approche "1 thread par connexion" (Apache 2.0)

```
Client 1  ──► Thread 1  (2 MB de stack par défaut)
Client 2  ──► Thread 2  (2 MB)
Client 3  ──► Thread 3  (2 MB)
...
Client 10 000 ──► Thread 10 000
```

**Coût en RAM :**
```
10 000 threads × 2 MB = 20 GB de RAM rien que pour les stacks 💀
```

**Coût en CPU (context switching) :**
```
Le noyau doit "tourner" entre 10 000 threads
Chaque switch : sauvegarder les registres CPU, vider le TLB, recharger
→ des centaines de millisecondes perdues en overhead
```

### La solution : I/O multiplexée

Au lieu d'un thread par connexion, un seul thread surveille **N connexions** et réagit quand l'une d'elles est prête.

```
1 thread
  │
  ├── surveille connexion 1, 2, 3, ..., 10 000
  │
  └── quand connexion 42 a des données → traite connexion 42
      quand connexion 1337 a des données → traite connexion 1337
      ...
```

C'est le modèle de **nginx** et de **Go net/http**.

---

<a name="epoll"></a>
## 3. epoll — multiplexage d'I/O

`epoll` est le mécanisme Linux (depuis 2.5.44, 2002) pour surveiller des milliers de file descriptors (fd) avec un seul thread.

### Évolution : select → poll → epoll

#### select (1983, BSD)
```c
fd_set fds;
FD_SET(fd1, &fds);
FD_SET(fd2, &fds);
select(max_fd + 1, &fds, NULL, NULL, &timeout);
// Problème : copie tout le set en espace noyau O(n)
// Limite : FD_SETSIZE = 1024 file descriptors max
```

#### poll (1997)
```c
struct pollfd fds[10000];
fds[0].fd = fd1;
poll(fds, 10000, timeout);
// Problème : toujours O(n) à chaque appel
// Pas de limite sur le nombre de fd
```

#### epoll (2002) — O(1) par événement
```c
// ① Créer une instance epoll (1 seule fois)
int epfd = epoll_create1(0);

// ② Enregistrer un fd à surveiller (1 seule fois par fd)
struct epoll_event ev;
ev.events = EPOLLIN;      // surveiller les données en entrée
ev.data.fd = client_fd;
epoll_ctl(epfd, EPOLL_CTL_ADD, client_fd, &ev);

// ③ Attendre des événements (bloque jusqu'à activité)
struct epoll_event events[64];
int n = epoll_wait(epfd, events, 64, -1);

// ④ Traiter les fd prêts
for (int i = 0; i < n; i++) {
    handle(events[i].data.fd);  // seulement les fd actifs
}
```

### Comment epoll est O(1)

```
select/poll : "parmi ces 10 000 fd, lesquels sont prêts ?"
→ le noyau scanne TOUS les 10 000 à chaque appel

epoll : le noyau maintient une liste interne (red-black tree)
→ quand un fd devient prêt, il est ajouté à une file d'attente
→ epoll_wait retourne UNIQUEMENT les fd prêts
```

```
Complexité select  : O(n) par appel
Complexité epoll   : O(1) par événement
                     O(log n) pour epoll_ctl (arbre rouge-noir)
```

### Go utilise epoll en coulisses

```go
// Ce code Go...
conn, err := listener.Accept()
go handleConn(conn)

// ...fait en coulisses :
// epoll_create1() au démarrage du runtime
// epoll_ctl(ADD, conn.fd)  quand Accept() retourne
// epoll_wait() dans le netpoller goroutine
// quand conn.fd est prêt → réveille la goroutine en attente sur Read()
```

Le **runtime Go** a un netpoller qui fait tourner epoll en arrière-plan. Chaque goroutine bloquée sur `conn.Read()` n'est **pas** bloquée dans le noyau — elle est parkée par le scheduler Go, et réveillée quand epoll signale que des données sont disponibles.

### EPOLLET — mode Edge-Triggered

```c
ev.events = EPOLLIN | EPOLLET;  // Edge-Triggered
// vs
ev.events = EPOLLIN;             // Level-Triggered (défaut)
```

| Mode | Comportement | Usage |
|------|--------------|-------|
| Level-Triggered | epoll_wait notifie **tant que** le fd est prêt | Plus simple, défaut |
| Edge-Triggered | epoll_wait notifie **une seule fois** quand le fd devient prêt | nginx, performances max |

Edge-Triggered force à tout lire d'un coup → moins d'appels epoll_wait → plus rapide.

---

<a name="io_uring"></a>
## 4. io_uring — I/O asynchrone sans syscall

`io_uring` est le mécanisme d'I/O asynchrone Linux depuis 5.1 (2019), conçu par Jens Axboe.

### Le problème des syscalls

Chaque opération I/O coûte un **changement de contexte** (user space → kernel space) :

```
read(fd, buf, n)
  │
  ├── sauvegarde registres CPU   (~100 ns)
  ├── switch vers kernel mode
  ├── vérifie permissions, copie buffer
  ├── switch vers user mode
  └── restore registres CPU      (~100 ns)
```

Pour 1 million d'opérations I/O par seconde : `1M × 200ns = 200ms` perdus en overhead.

### L'approche io_uring : ring buffers partagés

io_uring crée deux **ring buffers** partagés entre l'application et le noyau :

```
User space          Kernel space
     │                    │
     ▼                    ▼
┌─────────────────────────────────────────┐
│     Submission Queue (SQ)               │
│  [op:read, fd:5, buf:0x...] ←── user   │
│  [op:write, fd:3, buf:0x...]            │
│  [op:accept, fd:1, ...]                 │
└─────────────────────────────────────────┘
┌─────────────────────────────────────────┐
│     Completion Queue (CQ)               │
│  [result: 1024 bytes read]  ──► user   │
│  [result: 512 bytes written]            │
└─────────────────────────────────────────┘
```

**Sans io_uring :**
```
Pour 1000 opérations = 1000 syscalls = 1000 context switches
```

**Avec io_uring :**
```
① Remplir le SQ avec 1000 opérations (en user space, pas de syscall)
② io_uring_enter(1) — UN seul syscall pour soumettre tout
③ Le noyau exécute les 1000 opérations
④ Lire les résultats dans le CQ (en user space, pas de syscall)
```

### Modes d'opération

```
Mode 1 : interruptible
  → io_uring_enter() soumet + attend → comme epoll mais batched

Mode 2 : SQPOLL (sans syscall du tout)
  → thread kernel tourne en boucle et consomme le SQ
  → l'application écrit dans le SQ, le noyau lit automatiquement
  → 0 syscall pour des milliers d'opérations
```

### Opérations supportées

```
Réseau  : accept, recv, send, connect, sendmsg, recvmsg
Fichier : read, write, fsync, fallocate, rename, open
Timer   : timeout, link_timeout
Divers  : poll_add, cancel, provide_buffers
```

### io_uring en Go

```go
// Pas encore dans la stdlib, bibliothèques tierces :
// github.com/iceber/iouring-go
// github.com/pawelgaczynski/giouring
// github.com/dshulyak/uring

req := iouring.Read(fd, buf, 0)
result, err := ring.SubmitAndWait(req)
```

**Gains typiques :**
- Redis avec io_uring : +20% de throughput
- Nginx avec io_uring : +40% en zero-copy file serving
- fio benchmark : 3x plus d'IOPS vs epoll pour des petits fichiers

---

<a name="zero-copy"></a>
## 5. Zero-copy — sendfile et splice

Quand un serveur sert un fichier statique, l'approche naïve fait **4 copies** :

```
Disque ──► (1) Kernel buffer ──► (2) User buffer
       ──► (3) Kernel socket ──► (4) NIC (carte réseau)

Appels : read(fd, buf) → write(sockfd, buf)
Copies : disque → kernel RAM → user RAM → kernel socket → NIC
```

C'est du gaspillage : 2 copies inutiles (vers user space et retour).

### sendfile — 2 copies seulement

```c
// Copie directement du fd fichier vers le fd socket
sendfile(sockfd, filefd, &offset, count);
```

```
Disque ──► (1) Kernel buffer ──────────────► (2) NIC
                              DMA transfer

Appels : sendfile(sockfd, filefd, ...)
Copies : disque → kernel RAM → NIC
         (plus de passage par user space)
```

**En Go :**
```go
// http.ServeFile et os.File.WriteTo utilisent sendfile automatiquement
// quand la source est un *os.File et la destination est une *net.TCPConn

src, _ := os.Open("image.jpg")
dst, _ := net.Dial("tcp", "client:port")

// Go détecte src=*os.File + dst=*net.TCPConn → appelle sendfile(2) automatiquement
io.Copy(dst, src)
```

**nginx avec sendfile :**
```nginx
sendfile on;           # active sendfile(2)
tcp_nopush on;         # regroupe headers + début du fichier (TCP_CORK)
tcp_nodelay on;        # désactive Nagle pour les petits paquets finaux
```

### splice — entre deux fd quelconques

```c
// Copie entre deux fd sans passer par user space
// (pas forcément des fichiers, peut être pipes, sockets...)
splice(fd_in, NULL, fd_out, NULL, count, SPLICE_F_MOVE);
```

```
Socket in ──► (1) Kernel buffer ──► Socket out
             (référence kernel-to-kernel, pas de copie mémoire)
```

### Tableau comparatif

| Méthode | Copies mémoire | Syscalls | Meilleur pour |
|---------|---------------|----------|---------------|
| `read` + `write` | 4 | 2 | Cas général |
| `sendfile` | 2 | 1 | Fichier → socket |
| `splice` | 0–2 | 1 | Pipe → socket, socket → socket |
| `io_uring` + `FIXED_BUFFERS` | 1 | ~0 | I/O intensive |

---

<a name="mmap"></a>
## 6. mmap — fichiers en mémoire virtuelle

`mmap` mappe un fichier directement dans l'espace d'adressage du processus.

```c
void *addr = mmap(NULL, file_size, PROT_READ, MAP_SHARED, fd, 0);
// Maintenant addr[0..file_size] pointe directement sur le fichier
```

### Comment ça fonctionne

```
Mémoire virtuelle du processus
  ┌───────────────────────────────────┐
  │  code                             │
  │  heap                             │
  │  ...                              │
  │  0x7f3a00000000 ──► fichier.jpg  │  ← mmap
  └───────────────────────────────────┘

Noyau Linux
  ┌───────────────────────────────────┐
  │  Page Cache                       │
  │  fichier.jpg en RAM               │ ←─── même page physique
  └───────────────────────────────────┘
         │
         ▼ page fault si pas encore chargée
  ┌──────────────────┐
  │  Disque          │
  └──────────────────┘
```

La première lecture déclenche un **page fault** → le noyau charge la page du disque dans le Page Cache → la mappe dans le processus.

### Avantages

1. **Zéro copie** : les données du fichier ne sont jamais copiées en user space
2. **Lazy loading** : seules les pages réellement lues sont chargées
3. **Partage** : plusieurs processus peuvent mapper le même fichier → partage mémoire

### Cas d'usage en serveurs

```go
// PostgreSQL, SQLite, RocksDB utilisent mmap pour leurs fichiers de données

// En Go avec golang.org/x/exp/mmap :
r, _ := mmap.Open("large_file.bin")
buf := make([]byte, 1024)
r.ReadAt(buf, offset)  // lecture sans copie vers kernel
```

### Quand NE PAS utiliser mmap

- **Fichiers changeants** : cohérence cache complexe
- **Petits fichiers** : overhead de mmap > bénéfice
- **Random write** : amplification d'écriture

---

<a name="cpu-cache"></a>
## 7. Hiérarchie des caches CPU

Un accès RAM prend ~100 ns. Un calcul CPU prend ~0.3 ns. Sans cache, le CPU passerait 99% du temps à attendre la RAM.

### La hiérarchie

```
                    CPU (3.5 GHz)
                    ┌──────────────────────┐
Registres           │  0.3 ns    ~100 bytes │  (dans le processeur)
L1 Cache (data)     │  1 ns       32 KB     │  (par cœur)
L1 Cache (instr)    │  1 ns       32 KB     │  (par cœur)
L2 Cache            │  4 ns      256 KB     │  (par cœur)
L3 Cache (LLC)      │  10 ns     8–32 MB    │  (partagé entre cœurs)
                    └──────────────────────┘
RAM (DRAM)                100 ns    GB–TB
Disque SSD              100 000 ns  TB
```

### Cache line — l'unité de transfert

Le CPU ne transfère pas octet par octet, mais par blocs de **64 bytes** (cache line).

```go
// Exemple : parcourir un tableau de structs

type Data struct {
    X int64  // 8 bytes
    Y int64  // 8 bytes
}

// ❌ False sharing : X et Y dans la même cache line
// Si goroutine 1 écrit X et goroutine 2 écrit Y simultanément
// → elles invalident mutuellement la cache line de l'autre
// → performance similaire à un mutex !

// ✅ Padding pour éviter le false sharing
type DataPadded struct {
    X   int64
    _   [56]byte  // padding jusqu'à 64 bytes
    Y   int64
    _   [56]byte
}
```

### Localité spatiale et temporelle

```go
// ❌ MAUVAISE localité spatiale : sauts mémoire
for i := 0; i < n; i++ {
    sum += matrix[i][0]  // saute de ligne en ligne (non-contigu en mémoire)
}

// ✅ BONNE localité spatiale : accès séquentiels
for i := 0; i < n; i++ {
    for j := 0; j < m; j++ {
        sum += matrix[i][j]  // ligne par ligne = séquentiel en mémoire
    }
}
```

**Pour un serveur d'images :** parcourir les pixels ligne par ligne est toujours plus rapide que colonne par colonne, car les images sont stockées row-major.

### Préchargement (prefetching)

Le CPU détecte les accès séquentiels et précharge les cache lines à l'avance. Les accès aléatoires (linked lists, maps) ne bénéficient pas du prefetching.

```
Array (sequential) :  1 ns/op  (prefetch actif)
Linked list         : 50 ns/op  (pointer chasing, cache miss à chaque nœud)
```

---

<a name="numa"></a>
## 8. NUMA — accès mémoire non-uniforme

Sur les serveurs multi-socket, chaque CPU a sa propre RAM locale.

```
┌──────────────────────┐    ┌──────────────────────┐
│  Socket 0             │    │  Socket 1             │
│                       │    │                       │
│  CPU 0–15             │    │  CPU 16–31            │
│  RAM 0 : 64 GB        │◄──►│  RAM 1 : 64 GB        │
│  (accès local : 4 ns) │    │ (accès distant: 40 ns)│
└──────────────────────┘    └──────────────────────┘
         QPI / UPI interconnect
```

**Accès NUMA distant = 10x plus lent** que l'accès local.

```bash
# Afficher la topologie NUMA
numactl --hardware

# Forcer un processus sur le nœud NUMA 0 (RAM et CPU locaux)
numactl --cpunodebind=0 --membind=0 ./server

# Voir les statistiques NUMA
numastat
```

**Pour Docker :** sur un serveur NUMA, affecter les containers à un seul nœud NUMA évite les accès distants coûteux.

```bash
# Affecter un container au nœud NUMA 0
docker run --cpuset-cpus="0-15" --cpuset-mems="0" myapp
```

---

<a name="syscall"></a>
## 9. Appels système (syscall) — le coût caché

Un syscall traverse la frontière user space ↔ kernel space.

### Coût d'un syscall

```
Avant Spectre/Meltdown (2017) : ~100 ns
Après patches KPTI             : ~200–400 ns
```

**Les patches Spectre forcent un vidage du TLB** à chaque transition user↔kernel, ce qui multiplie le coût par 2–4.

### Syscalls courants dans un serveur HTTP

```
accept4()      ← nouvelle connexion TCP
read() / recv()← données du client
write() / send() ← réponse
close()        ← fermeture connexion
epoll_wait()   ← attente d'événements
```

### Voir les syscalls d'un processus

```bash
# Trace tous les syscalls du processus 1234
strace -p 1234

# Compter les syscalls par type
strace -c ./server 2>&1

# Exemples de sortie :
% time     seconds  usecs/call     calls    syscall
 45.12    0.042341         42      1003    epoll_wait
 20.33    0.019089         19      1003    read
 15.22    0.014291         14      1003    write
  9.89    0.009281          9      1003    sendfile
  ...
```

### Batching pour réduire les syscalls

```go
// ❌ Écriture octet par octet = des milliers de syscalls
for _, b := range data {
    conn.Write([]byte{b})
}

// ✅ Écriture en une fois = 1 syscall
conn.Write(data)

// ✅ bufio.Writer regroupe les petites écritures
bw := bufio.NewWriterSize(conn, 65536)
for _, line := range lines {
    bw.WriteString(line)
}
bw.Flush()  // 1 seul syscall
```

---

<a name="goroutine"></a>
## 10. Processus vs Thread vs Goroutine

```
Processus
  ├── espace mémoire isolé (4 GB virtual min)
  ├── création : fork() ≈ 1 ms
  ├── context switch : ≈ 1 µs + vidage TLB
  └── communication : IPC, socket, pipe

Thread OS (pthread)
  ├── partage la mémoire du processus
  ├── stack : 2 MB par défaut (configurable)
  ├── création : pthread_create ≈ 10 µs
  ├── context switch : ≈ 1 µs
  └── limite pratique : ~10 000 threads

Goroutine Go
  ├── partage la mémoire du processus
  ├── stack : 2–8 KB initial (grandit dynamiquement)
  ├── création : go f() ≈ 300 ns
  ├── context switch : ≈ 100 ns (géré par Go runtime)
  └── limite pratique : ~1 000 000 goroutines
```

### Scheduler M:N de Go

Go utilise un scheduler **M:N** (M goroutines sur N threads OS) :

```
Goroutines (M)          Threads OS (N)          CPUs
   G1                      P0                   CPU0
   G2    ──► scheduler ──► P1  ──► threads ──► CPU1
   G3                      P2                   CPU2
   ...                     P3                   CPU3
   G1000

M = 1 000 000 goroutines
N = GOMAXPROCS (= nombre de cœurs par défaut)
```

**P** = Processeur logique Go (chaque P tourne sur 1 thread OS)

Quand une goroutine fait un syscall bloquant (read, write), le scheduler déplace les autres goroutines vers un autre P → le thread ne reste pas bloqué.

```bash
# Voir le scheduler en action
GODEBUG=schedtrace=1000 ./server
# Affiche l'état du scheduler toutes les 1000ms
```

---

<a name="scratch"></a>
## 11. Docker FROM scratch — image minimale

**La question initiale : "le niveau zéro c'est faire un docker from scratch ?"**

Réponse : `FROM scratch` est une **image Docker vide**, pas une question de niveau 0 en Linux. C'est une optimisation de déploiement. La vraie "base zéro" Linux, c'est le kernel et ses primitives (epoll, io_uring, sendfile...).

### Qu'est-ce que FROM scratch ?

```dockerfile
# Une image "normale"
FROM ubuntu:22.04     # ≈ 77 MB (glibc, bash, apt, utils...)
COPY ./server /server
CMD ["/server"]

# FROM scratch = image complètement vide
FROM scratch          # 0 bytes, rien
COPY ./server /server
CMD ["/server"]
```

`FROM scratch` = aucun OS, aucune libc, aucun shell. Juste ton binaire.

### Pourquoi c'est possible avec Go ?

Go compile en **binaire statique** qui n'a pas besoin de libc dynamique.

```bash
# Compilation statique Go (sans dépendances externes)
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o server ./...

# Vérifier qu'il n'y a aucune dépendance dynamique
ldd ./server
# → not a dynamic executable ✅
```

```bash
# Comparaison des tailles d'images Docker

FROM ubuntu    + server binary = 77 MB + 15 MB = 92 MB
FROM alpine    + server binary = 5 MB  + 15 MB = 20 MB
FROM distroless + server binary = 2 MB + 15 MB = 17 MB
FROM scratch   + server binary = 0 MB  + 15 MB = 15 MB
```

### Dockerfile multi-stage (pattern habituel)

```dockerfile
# ── Stage 1 : Build ───────────────────────────────
FROM golang:1.23-alpine AS builder

WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o server .
#              ↑ pas de cgo     ↑ linux     ↑ strip debug symbols

# ── Stage 2 : Image finale ────────────────────────
FROM scratch

# Copier les certificats TLS (pour les appels HTTPS sortants)
COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/

# Copier le binaire
COPY --from=builder /app/server /server

EXPOSE 3000
ENTRYPOINT ["/server"]
```

**`-ldflags="-s -w"` :**
- `-s` : supprime la table des symboles (debug)
- `-w` : supprime les infos DWARF (débogage)
- Résultat : binaire 30–40% plus petit

### Ce que FROM scratch vous enlève

| Ce qu'on perd | Impact | Solution |
|---------------|--------|----------|
| Shell (`/bin/sh`) | Pas de `docker exec` interactif | Utiliser `docker cp` ou ajouter busybox |
| `/etc/passwd` | Pas d'utilisateur non-root par défaut | Créer l'utilisateur dans le builder, copier `/etc/passwd` |
| Certificats TLS | Appels HTTPS échouent | `COPY --from=builder /etc/ssl/certs/...` |
| Timezone data | `time.LoadLocation("Europe/Paris")` plante | `COPY --from=builder /usr/share/zoneinfo /usr/share/zoneinfo` |
| Libresolv | DNS peut échouer (rare) | Utiliser `CGO_ENABLED=0` correctement |

### Sécurité de FROM scratch

```
Surface d'attaque :
  Ubuntu image : bash + apt + curl + python + 400+ packages → 400+ vecteurs
  Scratch image : seulement votre binaire → 1 vecteur
```

Un attaquant qui exploite une vulnérabilité dans votre serveur ne trouve **aucun outil** sur le système : pas de shell, pas de curl, pas de wget, rien.

```dockerfile
# Ajouter un utilisateur non-root pour encore plus de sécurité
FROM scratch
COPY --from=builder /etc/passwd /etc/passwd
COPY --from=builder /app/server /server
USER nobody
ENTRYPOINT ["/server"]
```

### distroless — le compromis

`gcr.io/distroless/static` (Google) = scratch + certificats TLS + timezone + user nobody

```dockerfile
FROM gcr.io/distroless/static:nonroot
COPY --from=builder /app/server /server
ENTRYPOINT ["/server"]
```

Taille : ~2 MB. Plus facile à utiliser que FROM scratch.

---

<a name="projet"></a>
## 12. Primitives Linux dans notre projet

Cartographie des syscalls utilisés par notre stack :

```
┌─────────────────────────────────────────────────────┐
│                  front (nginx)                       │
│                                                     │
│  sendfile(2)     ← sert les fichiers statiques React│
│  epoll_wait(2)   ← surveille les connexions clients │
└──────────────────────┬──────────────────────────────┘
                       │ HTTP
┌──────────────────────▼──────────────────────────────┐
│                  api (Go)                            │
│                                                     │
│  accept4(2)      ← nouvelles connexions HTTP        │
│  epoll_wait(2)   ← netpoller Go                     │
│  read(2)         ← lecture corps multipart          │
│  write(2)        ← écriture réponse JSON/image      │
│  futex(2)        ← sync.Mutex, channel operations   │
│  clone(2)        ← goroutines (via threads OS)      │
└──────────────────────┬──────────────────────────────┘
                       │ HTTP (io.Pipe)
┌──────────────────────▼──────────────────────────────┐
│              optimizer (Go)                          │
│                                                     │
│  accept4(2)      ← connexions depuis l'API          │
│  read(2)         ← lecture image multipart          │
│  write(2)        ← écriture image JPEG              │
│  madvise(2)      ← hints mémoire pour image.RGBA    │
└─────────────────────────────────────────────────────┘

Infrastructure :
  Redis  ← epoll, sendfile pour BGSAVE
  MinIO  ← sendfile pour objets disque → socket
  RabbitMQ ← epoll, writev pour batching messages
```

### Voir les syscalls en pratique

```bash
# Tracer l'API pendant un upload
docker exec watermark-api-1 strace -p 1 -e trace=read,write,sendfile -c

# Compter les appels epoll
docker exec watermark-optimizer-1 strace -p 1 -e trace=epoll_wait -c 2>&1 | grep epoll

# Voir les file descriptors ouverts
ls -la /proc/$(pgrep server)/fd
```

---

<a name="outils"></a>
## 13. Outils de diagnostic Linux

### perf — profiling système

```bash
# Profiler les hot spots CPU de l'API
perf record -g -p $(pgrep api) -- sleep 30
perf report

# Compter les cache misses
perf stat -e cache-misses,cache-references ./server

# Flamegraph depuis perf
perf record -F 99 -g ./server &
sleep 30 && kill %1
perf script | stackcollapse-perf.pl | flamegraph.pl > flame.svg
```

### flamegraph avec Go pprof

```go
// Ajouter au serveur
import _ "net/http/pprof"

// http://localhost:6060/debug/pprof/goroutine
// http://localhost:6060/debug/pprof/heap
```

```bash
# Générer un flamegraph
go tool pprof -http=:8080 http://localhost:6060/debug/pprof/profile?seconds=30
```

### vmstat / iostat — vue d'ensemble

```bash
# I/O disque et CPU toutes les secondes
vmstat 1

# I/O par device
iostat -x 1

# Mémoire virtuelle et swapping
cat /proc/meminfo
```

### ss — connexions réseau (remplace netstat)

```bash
# Toutes les connexions TCP
ss -tnp

# Connexions en ESTABLISHED vers le port 3000
ss -tnp state established '( dport = :3000 or sport = :3000 )'

# Statistiques TCP
ss -s
```

### /proc — tout est un fichier

```bash
# Statistiques réseau du processus
cat /proc/$(pgrep api)/net/dev

# Consommation mémoire détaillée
cat /proc/$(pgrep api)/status

# Syscalls depuis le démarrage
cat /proc/$(pgrep api)/syscall

# Affinité CPU
taskset -p $(pgrep api)
```

---

## 📊 Résumé

| Primitive | Problème résolu | Gain |
|-----------|----------------|------|
| **epoll** | 1 thread peut gérer 100K connexions | scalabilité ×100 |
| **io_uring** | Réduction des syscalls I/O | +20–40% throughput |
| **sendfile** | Copie fichier → socket sans user space | ×2 vitesse serveur statique |
| **mmap** | Fichiers sans copie en user space | 0 copie, lazy load |
| **Cache CPU** | Localité des données | ×10–50 selon algo |
| **FROM scratch** | Image Docker minimale | surface attaque ×0.01 |

---

## 🔗 Pour aller plus loin

- **"The Linux Programming Interface"** — Michael Kerrisk (bible de l'OS Linux)
- **"Systems Performance"** — Brendan Gregg (perf, flamegraph, Linux internals)
- `man 7 epoll`, `man 2 io_uring_setup`, `man 2 sendfile`
- **Linux kernel source** : `fs/read_write.c` (sendfile), `fs/io_uring.c`
- **"What every programmer should know about memory"** — Ulrich Drepper (CPU cache)

---

*"Un programme qui ne comprend pas l'OS qu'il tourne dessus laisse des performances sur la table."*
