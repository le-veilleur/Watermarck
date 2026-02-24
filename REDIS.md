# Cours : Redis
## Base de données en mémoire, cache et broker

---

## 📋 Table des matières

1. [C'est quoi Redis ?](#intro)
2. [Les structures de données](#structures)
3. [TTL — Expiration automatique](#ttl)
4. [Persistance — Ne pas perdre les données](#persistance)
5. [Cache — Les patterns classiques](#cache)
6. [Pub/Sub — Messagerie légère](#pubsub)
7. [Transactions — MULTI/EXEC](#transactions)
8. [Pipelines — Grouper les commandes](#pipelines)
9. [Redis Sentinel — Haute disponibilité](#sentinel)
10. [Redis Cluster — Scalabilité horizontale](#cluster)
11. [Résumé et cas d'usage](#résumé)

---

<a name="intro"></a>
## 1. C'est quoi Redis ?

**Redis** = **RE**mote **DI**ctionary **S**erver

C'est une base de données **clé-valeur** qui stocke tout **en RAM**.

**Analogie :** C'est comme un dictionnaire géant ultra-rapide posé sur la table devant toi — au lieu d'aller chercher l'info dans une armoire (disque), tu la prends directement devant toi (RAM).

```
RAM   : ~0.1 µs pour lire 1 KB  ← Redis
Disque SSD : ~100 µs pour lire 1 KB  ← MySQL, PostgreSQL
```

**RAM = 1000x plus rapide que le disque**

---

### Ce que Redis n'est pas

Redis n'est **pas** une base de données relationnelle. Pas de tables, pas de SQL, pas de JOIN.

| | Redis | PostgreSQL |
|---|-------|-----------|
| Stockage | RAM | Disque |
| Structure | Clé-valeur | Tables relationnelles |
| Requêtes | Commandes simples | SQL |
| Vitesse | < 1ms | ~5-50ms |
| Persistance | Optionnelle | Oui |
| Use case | Cache, sessions, compteurs | Données métier complexes |

---

### Démarrer Redis

```bash
# Lancer Redis avec Docker
docker run -d -p 6379:6379 redis:alpine

# Se connecter en CLI
redis-cli

# Commandes de base
SET nom "Alice"       # stocker
GET nom               # lire       → "Alice"
DEL nom               # supprimer
EXISTS nom            # existe ?   → 0 ou 1
KEYS *                # toutes les clés (⚠ jamais en prod)
```

---

<a name="structures"></a>
## 2. Les structures de données

Redis ne stocke pas que des chaînes. Il supporte 7 types de données natifs.

---

### 2a. String — La base

Le type le plus simple : une clé → une valeur (texte, nombre, binaire).

```bash
SET user:1:name "Alice"
GET user:1:name            # → "Alice"

SET compteur 0
INCR compteur              # → 1  (atomique !)
INCR compteur              # → 2
INCRBY compteur 10         # → 12
DECR compteur              # → 11

# Stocker un JSON
SET user:1 '{"name":"Alice","age":30}'
GET user:1                 # → '{"name":"Alice","age":30}'

# Stocker avec expiration
SETEX session:abc123 3600 "user_id=42"   # expire dans 3600s
```

**Use case :** Cache de requêtes, compteurs de vues, sessions utilisateur.

---

### 2b. List — File et pile

Une liste ordonnée de chaînes. On peut ajouter/retirer **au début ou à la fin** en O(1).

```bash
# Ajouter à droite (fin de liste)
RPUSH notifications "Nouveau message"
RPUSH notifications "Commande livrée"
RPUSH notifications "Paiement reçu"

# Lire la liste (0 = début, -1 = fin)
LRANGE notifications 0 -1
# → 1) "Nouveau message"
#    2) "Commande livrée"
#    3) "Paiement reçu"

# Retirer le premier élément (FIFO)
LPOP notifications    # → "Nouveau message"

# Retirer le dernier (LIFO / pile)
RPOP notifications    # → "Paiement reçu"

# Taille de la liste
LLEN notifications    # → 1
```

**LPUSH + RPOP = file d'attente (FIFO)**
**LPUSH + LPOP = pile (LIFO)**

**Use case :** File de tâches légères, historique d'activité, notifications.

```go
// Producteur
redisClient.RPush(ctx, "tasks", `{"type":"email","to":"alice@example.com"}`)

// Consommateur (bloque jusqu'à un message)
result, err := redisClient.BLPop(ctx, 0, "tasks").Result()
if err == nil {
    fmt.Println(result[1]) // result[0] = nom de la queue, result[1] = valeur
}
```

---

### 2c. Hash — Objet structuré

Un hash est une **map de champs** sous une seule clé. Idéal pour représenter un objet.

```bash
# Stocker un utilisateur
HSET user:42 name "Alice" email "alice@example.com" age 30 role "admin"

# Lire un champ
HGET user:42 name          # → "Alice"

# Lire tous les champs
HGETALL user:42
# → 1) "name"
#    2) "Alice"
#    3) "email"
#    4) "alice@example.com"
#    5) "age"
#    6) "30"
#    7) "role"
#    8) "admin"

# Modifier un champ
HSET user:42 age 31

# Incrémenter un champ numérique
HINCRBY user:42 age 1      # → 32

# Supprimer un champ
HDEL user:42 role

# Vérifier l'existence d'un champ
HEXISTS user:42 email      # → 1
```

**Avantage vs String :** Modifier un champ ne nécessite pas de lire/réécrire tout l'objet.

**Use case :** Profils utilisateur, paramètres de configuration, état de session détaillé.

---

### 2d. Set — Ensemble sans doublons

Un set est une **collection non ordonnée** de chaînes uniques.

```bash
# Ajouter des membres
SADD tags:article:1 "go" "performance" "backend"
SADD tags:article:1 "go"  # doublon → ignoré

# Vérifier l'appartenance
SISMEMBER tags:article:1 "go"        # → 1
SISMEMBER tags:article:1 "python"    # → 0

# Tous les membres
SMEMBERS tags:article:1
# → 1) "go"
#    2) "performance"
#    3) "backend"

# Opérations ensemblistes
SADD tags:article:2 "go" "cloud" "docker"

SINTER tags:article:1 tags:article:2   # intersection → "go"
SUNION tags:article:1 tags:article:2   # union → tous les tags
SDIFF  tags:article:1 tags:article:2   # différence → "performance", "backend"
```

**Use case :** Tags, liste d'amis communs, IPs uniques par jour, permissions.

---

### 2e. Sorted Set — Ensemble ordonné par score

Comme un Set, mais chaque membre a un **score numérique**. L'ordre est maintenu automatiquement.

```bash
# Ajouter avec score
ZADD leaderboard 1500 "Alice"
ZADD leaderboard 2300 "Bob"
ZADD leaderboard 1800 "Charlie"
ZADD leaderboard 2100 "Diana"

# Classement (ordre croissant)
ZRANGE leaderboard 0 -1 WITHSCORES
# → Alice 1500, Charlie 1800, Diana 2100, Bob 2300

# Classement (ordre décroissant = du meilleur)
ZREVRANGE leaderboard 0 -1 WITHSCORES
# → Bob 2300, Diana 2100, Charlie 1800, Alice 1500

# Top 3
ZREVRANGE leaderboard 0 2

# Score d'un membre
ZSCORE leaderboard "Alice"     # → 1500

# Rang d'un membre (0-indexed)
ZREVRANK leaderboard "Bob"     # → 0 (1er)
ZREVRANK leaderboard "Alice"   # → 3 (4ème)

# Incrémenter le score
ZINCRBY leaderboard 500 "Alice"  # Alice passe à 2000
```

**Use case :** Classements, priorités de tâches, timeline triée par timestamp, rate limiting.

---

### 2f. Stream — Journal d'événements

Un stream est un **log immuable** d'entrées horodatées. C'est Redis en mode "Kafka léger".

```bash
# Ajouter un événement (ID auto-généré)
XADD events * action "login" user "alice" ip "192.168.1.1"
# → "1706789012345-0"  (timestamp-séquence)

XADD events * action "purchase" user "alice" amount "99.90"

# Lire les événements
XRANGE events - +      # tous les événements
XRANGE events - + COUNT 10  # 10 premiers

# Lire en temps réel (attend les nouveaux messages)
XREAD BLOCK 0 STREAMS events $

# Groupes de consommateurs (comme RabbitMQ)
XGROUP CREATE events processors $ MKSTREAM
XREADGROUP GROUP processors worker1 COUNT 1 STREAMS events >
```

**Use case :** Audit log, événements temps réel, remplacement léger de Kafka.

---

### 📊 Comparaison des structures

| Structure | Opérations clés | Use case |
|-----------|----------------|----------|
| **String** | GET/SET/INCR | Cache, compteurs, sessions |
| **List** | LPUSH/RPOP/LRANGE | Files de tâches, historique |
| **Hash** | HGET/HSET/HGETALL | Objets, profils utilisateur |
| **Set** | SADD/SISMEMBER/SINTER | Tags, dédoublonnage, permissions |
| **Sorted Set** | ZADD/ZRANGE/ZSCORE | Classements, rate limiting |
| **Stream** | XADD/XRANGE/XREAD | Événements, audit log |

---

<a name="ttl"></a>
## 3. TTL — Expiration automatique

Redis peut **supprimer automatiquement** une clé après un délai. C'est le TTL (Time To Live).

```bash
# Définir un TTL en secondes
EXPIRE session:abc123 3600       # expire dans 1 heure

# Définir lors de la création
SETEX cache:user:42 300 "données"  # expire dans 5 minutes

# Voir le TTL restant
TTL session:abc123    # → 3542 (secondes restantes)
TTL session:xyz       # → -1  (pas d'expiration)
TTL session:old       # → -2  (clé inexistante ou expirée)

# Supprimer l'expiration
PERSIST session:abc123   # → clé devient permanente

# TTL en millisecondes
PEXPIRE clé 5000         # expire dans 5000ms
PTTL clé                 # TTL restant en ms
```

---

### Comment fonctionne l'expiration ?

Redis utilise deux stratégies combinées :

**1. Lazy expiration :** La clé est supprimée seulement quand on essaie d'y accéder.
```
GET session:expired → Redis vérifie le TTL → clé expirée → supprime → retourne nil
```

**2. Active expiration :** Redis scanne régulièrement un échantillon de clés pour supprimer celles expirées en arrière-plan.

**Conséquence :** Une clé expirée n'est pas forcément supprimée immédiatement — mais elle n'est plus accessible.

---

<a name="persistance"></a>
## 4. Persistance — Ne pas perdre les données

Par défaut Redis stocke tout en RAM. Si Redis redémarre → **tout est perdu**. Deux mécanismes permettent de persister sur disque.

---

### 4a. RDB — Snapshot (photo instantanée)

Redis prend une **photo de toutes les données** à intervalles réguliers et l'écrit sur disque.

```
t=0h    Données en RAM : {A, B, C}
t=1h    RDB snapshot → dump.rdb écrit sur disque
t=2h    Nouvelles données : {A, B, C, D, E}
t=2h30  CRASH
t=2h31  Redis redémarre → charge dump.rdb → {A, B, C} ← D et E sont perdus !
```

**Configuration (redis.conf) :**
```
save 3600 1     # snapshot si 1 changement en 1h
save 300 100    # snapshot si 100 changements en 5min
save 60 10000   # snapshot si 10000 changements en 1min
```

**Avantages :** Compact, rapide au redémarrage, idéal pour les sauvegardes.
**Inconvénient :** Perte des données entre deux snapshots.

---

### 4b. AOF — Append Only File (journal)

Redis enregistre **chaque commande d'écriture** dans un fichier log.

```
Commande SET user:1 "Alice" → écrite dans appendonly.aof
Commande SET user:2 "Bob"   → écrite dans appendonly.aof
Commande DEL user:1          → écrite dans appendonly.aof

Au redémarrage : Redis rejoue toutes les commandes du fichier
→ Aucune perte de données !
```

**Modes de synchronisation :**
```
appendfsync always    # écrit à chaque commande → 0 perte, lent
appendfsync everysec  # écrit toutes les secondes → max 1s de perte, rapide ✅
appendfsync no        # laisse l'OS décider → rapide, risqué
```

**Réécriture AOF :** Le fichier grossit indéfiniment → Redis le compacte régulièrement.
```
BGREWRITEAOF  # déclenche manuellement la réécriture
```

---

### 4c. Comparaison RDB vs AOF

| Critère | RDB | AOF |
|---------|-----|-----|
| Perte de données max | Depuis le dernier snapshot | ~1 seconde (everysec) |
| Vitesse de redémarrage | Rapide | Lent (rejoue les commandes) |
| Taille fichier | Compact | Plus volumineux |
| Impact performance | Faible | Très faible (everysec) |
| Use case | Sauvegardes, dev | Production critique |

**Recommandation :** Activer **les deux** en production — RDB pour les sauvegardes, AOF pour la durabilité.

```bash
# Dans notre docker-compose, Redis est configuré sans persistance
# car utilisé uniquement comme cache (les données peuvent être perdues)
command: redis-server --save "" --appendonly no
```

---

<a name="cache"></a>
## 5. Cache — Les patterns classiques

---

### 5a. Cache-Aside (Lazy Loading)

Le pattern le plus courant. L'application gère elle-même le cache.

```
Lecture :
  App → Redis.Get(clé)
    ├── HIT  → retourne la valeur depuis Redis
    └── MISS → App lit depuis la DB → App.Set(clé, valeur, TTL) → retourne

Écriture :
  App → DB.Update(donnée) → Redis.Del(clé)  ← invalide le cache
```

```go
// Notre implémentation dans api/main.go
cached, err := redisClient.Get(ctx, cacheKey).Bytes()
if err == nil {
    return cached  // HIT
}
// MISS → traitement → stockage
result := processImage(data)
redisClient.Set(ctx, cacheKey, result, 24*time.Hour)
return result
```

**Avantages :** Simple, flexible, le cache ne contient que ce qui est demandé.
**Inconvénient :** La 1ère requête est toujours lente (cache miss).

---

### 5b. Write-Through

À chaque écriture en base, on met à jour le cache **simultanément**.

```
App → DB.Write(donnée) → Redis.Set(clé, donnée)
```

**Avantage :** Cache toujours à jour, jamais de miss sur des données récentes.
**Inconvénient :** Écriture plus lente, cache peut contenir des données jamais relues.

---

### 5c. Write-Behind (Write-Back)

L'application écrit **d'abord dans Redis**, puis Redis persiste en DB de façon asynchrone.

```
App → Redis.Set(clé, donnée) → retourne immédiatement
                              ↓ (asynchrone)
                          DB.Write(donnée)
```

**Avantage :** Écriture ultra-rapide.
**Inconvénient :** Risque de perte si Redis crashe avant la persistence.

---

### 5d. Cache Stampede (thundering herd)

**Le problème :** 1000 requêtes arrivent au même moment sur une clé expirée → 1000 requêtes vont en DB simultanément → surcharge.

```
TTL expire à t=10h00
1000 requêtes à t=10h00:001 → toutes font Redis.Get → MISS
                            → toutes vont en DB → 💥
```

**Solution : mutex ou probabilistic early expiration**

```go
// Mutex simple avec SETNX (Set if Not eXists)
locked := redisClient.SetNX(ctx, cacheKey+":lock", "1", 10*time.Second)
if locked.Val() {
    // Seul ce thread recalcule
    result := processImage(data)
    redisClient.Set(ctx, cacheKey, result, 24*time.Hour)
    redisClient.Del(ctx, cacheKey+":lock")
} else {
    // Les autres attendent
    time.Sleep(100 * time.Millisecond)
    result = redisClient.Get(ctx, cacheKey).Bytes()
}
```

---

<a name="pubsub"></a>
## 6. Pub/Sub — Messagerie légère

Redis permet de faire de la messagerie **publish/subscribe** en temps réel.

```
Subscriber A ─┐
Subscriber B ─┼── écoute canal "notifications"
Subscriber C ─┘

Publisher ──► PUBLISH notifications "Nouveau message"
             → reçu instantanément par A, B et C
```

```bash
# Subscriber (terminal 1)
SUBSCRIBE notifications
# → Waiting for messages...

# Publisher (terminal 2)
PUBLISH notifications "Bonjour !"
# → Subscriber reçoit : "Bonjour !"
```

```go
// Subscriber
pubsub := redisClient.Subscribe(ctx, "notifications")
defer pubsub.Close()

ch := pubsub.Channel()
for msg := range ch {
    fmt.Println(msg.Payload) // message reçu
}

// Publisher
redisClient.Publish(ctx, "notifications", "Bonjour !")
```

---

### Pattern matching sur les canaux

```bash
PSUBSCRIBE order.*        # écoute order.created, order.paid, order.shipped...
PSUBSCRIBE log.*error*    # tous les canaux contenant "error"
```

---

### ⚠️ Limitations du Pub/Sub Redis

| Limitation | Conséquence |
|-----------|-------------|
| Pas de persistance | Si le subscriber est offline → message perdu |
| Pas d'ACK | Pas de garantie de livraison |
| Pas d'historique | Impossible de rejouer les messages |

**Pour des messages critiques → utiliser Redis Streams ou RabbitMQ.**
**Redis Pub/Sub = notifications temps réel non critiques** (ex: mise à jour live d'une UI).

---

<a name="transactions"></a>
## 7. Transactions — MULTI/EXEC

Redis permet d'exécuter un **groupe de commandes de façon atomique** — soit toutes réussissent, soit aucune n'est exécutée.

```bash
MULTI           # début de transaction
SET compte:alice 100
SET compte:bob 200
INCRBY compte:alice -50
INCRBY compte:bob 50
EXEC            # exécute toutes les commandes atomiquement
```

```
Sans transaction :
  INCRBY compte:alice -50   → alice = 50
  [CRASH]
  INCRBY compte:bob 50      → jamais exécuté → 50€ disparaissent 💀

Avec transaction :
  MULTI
  INCRBY compte:alice -50
  INCRBY compte:bob 50
  EXEC → les deux s'exécutent ou aucun
```

---

### WATCH — Transaction optimiste

`WATCH` permet d'annuler la transaction si une clé a été modifiée entre-temps.

```bash
WATCH compte:alice         # surveille la clé
MULTI
INCRBY compte:alice -50    # si alice a été modifié par quelqu'un d'autre...
INCRBY compte:bob 50
EXEC                       # → nil (transaction annulée) ou succès
```

**Analogie :** C'est comme un optimistic lock en base de données.

---

<a name="pipelines"></a>
## 8. Pipelines — Grouper les commandes

Par défaut, chaque commande Redis fait un **aller-retour réseau** :

```
Client ──► GET clé1 ──► Redis ──► réponse1 ──► Client  (~1ms)
Client ──► GET clé2 ──► Redis ──► réponse2 ──► Client  (~1ms)
Client ──► GET clé3 ──► Redis ──► réponse3 ──► Client  (~1ms)
Total : ~3ms
```

Avec un pipeline, toutes les commandes sont **envoyées en une seule fois** :

```
Client ──► GET clé1, GET clé2, GET clé3 ──► Redis ──► réponse1, réponse2, réponse3 ──► Client
Total : ~1ms
```

```go
// Sans pipeline : 3 allers-retours réseau
redisClient.Get(ctx, "clé1")
redisClient.Get(ctx, "clé2")
redisClient.Get(ctx, "clé3")

// Avec pipeline : 1 seul aller-retour réseau
pipe := redisClient.Pipeline()
get1 := pipe.Get(ctx, "clé1")
get2 := pipe.Get(ctx, "clé2")
get3 := pipe.Get(ctx, "clé3")
pipe.Exec(ctx)

fmt.Println(get1.Val(), get2.Val(), get3.Val())
```

**Gain :** Sur 100 commandes → **100x moins d'allers-retours réseau**.

---

<a name="sentinel"></a>
## 9. Redis Sentinel — Haute disponibilité

En production, un seul Redis est un **point de défaillance unique** (SPOF). Redis Sentinel surveille le serveur et bascule automatiquement en cas de panne.

```
┌─────────────┐    surveille    ┌──────────────┐
│  Sentinel 1 │────────────────►│  Redis Master │◄── Écriture
│  Sentinel 2 │────────────────►│  (primaire)   │
│  Sentinel 3 │                 └──────┬────────┘
└─────────────┘                        │ réplication
                                       ▼
                               ┌───────────────┐
                               │  Redis Replica │◄── Lecture
                               │  (secondaire)  │
                               └───────────────┘
```

**En cas de panne du Master :**

```
Master KO → Sentinels détectent la panne (quorum)
          → Sentinel élit un nouveau Master parmi les Replicas
          → Clients redirigés vers le nouveau Master
          → Durée de basculement : ~30 secondes
```

**Configuration minimale :** 3 Sentinels (quorum = 2).

---

<a name="cluster"></a>
## 10. Redis Cluster — Scalabilité horizontale

Redis est **mono-thread** pour les commandes — il ne peut utiliser qu'un seul cœur CPU. Redis Cluster permet de **distribuer les données sur plusieurs nœuds**.

### Sharding par hash slot

Redis Cluster divise les données en **16 384 hash slots**.

```
Hash slot d'une clé : CRC16(clé) % 16384

Nœud A : slots 0     → 5460     (1/3 des données)
Nœud B : slots 5461  → 10922    (1/3 des données)
Nœud C : slots 10923 → 16383    (1/3 des données)
```

```
SET user:42 "Alice"
→ CRC16("user:42") % 16384 = 4821
→ stocké sur Nœud A (slots 0-5460) ✅
```

### Architecture Cluster

```
┌──────────┐  ┌──────────┐  ┌──────────┐   ← Masters
│  Nœud A  │  │  Nœud B  │  │  Nœud C  │
│ slots    │  │ slots    │  │ slots    │
│ 0-5460   │  │ 5461-10922│  │10923-16383│
└────┬─────┘  └────┬─────┘  └────┬─────┘
     │              │              │          ← Réplication
┌────▼─────┐  ┌────▼─────┐  ┌────▼─────┐
│ Replica A│  │ Replica B│  │ Replica C│   ← Replicas
└──────────┘  └──────────┘  └──────────┘
```

**Minimum :** 3 masters + 3 replicas = 6 nœuds.

---

### Sentinel vs Cluster

| | Sentinel | Cluster |
|---|---------|---------|
| Objectif | Haute disponibilité | Scalabilité + disponibilité |
| Sharding | Non (1 nœud = toutes les données) | Oui (données distribuées) |
| Complexité | Faible | Élevée |
| Quand l'utiliser | ≤ quelques GB de données | > RAM d'un seul serveur |

---

<a name="résumé"></a>
## 11. 📊 Résumé et cas d'usage

### Les structures en un coup d'œil

```
String     → clé : valeur simple
             SET user:1 "Alice" / GET user:1

List       → clé : [v1, v2, v3, ...]   (file / pile)
             RPUSH tasks "job" / LPOP tasks

Hash       → clé : {champ: valeur}     (objet)
             HSET user:1 name "Alice" age 30

Set        → clé : {v1, v2, v3}        (unique, non ordonné)
             SADD tags "go" "backend"

Sorted Set → clé : {membre: score}     (unique, ordonné)
             ZADD scores 1500 "Alice"

Stream     → clé : [(id, champs), ...]  (log immuable)
             XADD events * action "login"
```

---

### Cas d'usage classiques

| Cas d'usage | Structure | Commandes clés |
|-------------|-----------|----------------|
| Cache de requêtes API | String | GET / SET + TTL |
| Session utilisateur | Hash | HGETALL / HSET |
| File de tâches | List | RPUSH / BLPOP |
| Compteur de vues | String | INCR |
| Classement temps réel | Sorted Set | ZADD / ZREVRANGE |
| Dédoublonnage | Set | SADD / SISMEMBER |
| Rate limiting | String + TTL | INCR / EXPIRE |
| Pub/Sub temps réel | Pub/Sub | PUBLISH / SUBSCRIBE |
| Audit log | Stream | XADD / XRANGE |
| Lock distribué | String | SETNX + TTL |

---

### Rate Limiting avec Redis

Un cas d'usage très courant : limiter les requêtes d'un utilisateur.

```go
// Max 100 requêtes par minute par IP
func isRateLimited(ip string) bool {
    key := "rate:" + ip
    count, _ := redisClient.Incr(ctx, key).Result()
    if count == 1 {
        redisClient.Expire(ctx, key, time.Minute)  // démarre le compteur
    }
    return count > 100
}
```

```
1ère requête : INCR rate:192.168.1.1 → 1, EXPIRE 60s
50ème requête : INCR → 50
100ème requête : INCR → 100
101ème requête : INCR → 101 → bloqué ❌
t+60s : clé expire → compteur reset → autorisé ✅
```

---

### Lock distribué (Redlock)

Empêcher deux instances de faire la même chose en parallèle.

```go
// Acquérir le lock (SETNX = SET if Not eXists)
acquired := redisClient.SetNX(ctx, "lock:job:42", "worker-1", 30*time.Second)

if acquired.Val() {
    // On a le lock → on fait le travail
    processJob(42)
    // Libérer le lock
    redisClient.Del(ctx, "lock:job:42")
} else {
    // Quelqu'un d'autre traite déjà ce job
    log.Println("Job 42 déjà en cours de traitement")
}
```

---

### Commandes d'inspection utiles

```bash
# Info générale
INFO server
INFO memory
INFO stats

# Mémoire utilisée
INFO memory | grep used_memory_human

# Nombre de clés
DBSIZE

# Surveiller en temps réel
MONITOR          # ⚠ très verbeux, jamais en prod

# Statistiques de latence
LATENCY LATEST

# Clés par pattern (⚠ bloquant sur gros datasets, utiliser SCAN)
SCAN 0 MATCH user:* COUNT 100

# Supprimer toutes les données (⚠ irréversible)
FLUSHDB          # vide la base courante
FLUSHALL         # vide toutes les bases
```

---

### Concepts clés à retenir

#### 1. **RAM = vitesse**
Redis est rapide parce qu'il lit et écrit en mémoire. La persistance (RDB/AOF) est optionnelle et découplée.

#### 2. **Choisir la bonne structure**
Chaque structure a ses opérations optimales. Un Hash pour un objet, un Sorted Set pour un classement, une List pour une queue.

#### 3. **TTL = gestion de la mémoire**
Sans TTL, Redis remplit la RAM indéfiniment. Toujours définir une expiration sur les données temporaires.

#### 4. **Cache-Aside = le pattern universel**
Lire depuis Redis, fallback sur la DB si miss, stocker dans Redis avec TTL. C'est le pattern le plus utilisé.

#### 5. **Atomic = thread-safe**
INCR, SETNX, GETSET... Ces commandes sont atomiques. Exploite-les pour les compteurs et les locks sans avoir besoin de mutex applicatif.

---

## 📚 Pour aller plus loin

- **RedisInsight** : UI desktop pour visualiser et déboguer Redis
- **Redis modules** : RedisSearch (full-text), RedisJSON (stockage JSON natif), RedisTimeSeries
- **ioredis / go-redis** : clients Redis pour Node.js et Go
- **Lettuce** : client Redis réactif pour Java
- **RESP3** : nouveau protocole Redis avec des types de données enrichis

---

**🎓 Fin du cours — Redis**
