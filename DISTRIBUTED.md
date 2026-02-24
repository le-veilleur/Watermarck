# Cours : Architecture Distribuée
## Scaler horizontalement — Load Balancing, DLQ, Consistent Hashing, CQRS

---

## 📋 Table des matières

1. [C'est quoi l'architecture distribuée ?](#intro)
2. [Le théorème CAP](#cap)
3. [Scaling vertical vs horizontal](#scaling)
4. [Load Balancing — distribuer la charge](#load-balancing)
5. [Service Discovery — trouver les services](#discovery)
6. [Dead Letter Queue — ne pas perdre les messages](#dlq)
7. [Consistent Hashing — distribuer sans tout casser](#hashing)
8. [Distributed Locks — synchronisation cross-services](#locks)
9. [Saga Pattern — transactions distribuées](#saga)
10. [CQRS — séparer lectures et écritures](#cqrs)
11. [Event Sourcing — stocker les événements](#event-sourcing)
12. [Utilisation dans NWS Watermark](#watermark)
13. [Résumé](#résumé)

---

<a name="intro"></a>
## 1. C'est quoi l'architecture distribuée ?

Un système distribué = plusieurs processus sur plusieurs machines qui coopèrent pour accomplir une tâche.

**Analogie :** une chaîne de restaurants.
- Un seul restaurant = système centralisé (si ça ferme, plus rien)
- 50 restaurants dans la ville = système distribué (si l'un ferme, les autres servent)

```
Système centralisé :              Système distribué :
┌──────────────────┐              ┌────┐ ┌────┐ ┌────┐
│   API monolithique│              │API │ │API │ │API │
│   + Optimizer     │              └────┘ └────┘ └────┘
│   + Cache         │              ┌────┐ ┌────┐
│   + Storage       │              │OPT │ │OPT │
└──────────────────┘              └────┘ └────┘
1 point de défaillance            Résilience, scalabilité
```

### Les défis propres aux systèmes distribués

1. **Latence réseau** — les appels réseau sont 1000x plus lents que la mémoire
2. **Défaillances partielles** — un service peut être lent sans être mort
3. **Données incohérentes** — deux services peuvent avoir des visions différentes de la réalité
4. **Ordre des événements** — deux événements simultanés peuvent arriver dans n'importe quel ordre
5. **Split brain** — deux parties du système peuvent se croire chacune "le vrai leader"

---

<a name="cap"></a>
## 2. Le théorème CAP

**Eric Brewer, 2000 :** un système distribué ne peut garantir que **2 des 3 propriétés** suivantes simultanément.

```
         Cohérence (C)
         "tous les noeuds voient la même donnée"
              /\
             /  \
            /    \
           /      \
          /        \
Disponibilité ──── Tolérance aux partitions
     (A)                  (P)
"répond toujours"    "résiste aux coupures réseau"
```

**CP — Cohérence + Partition tolerance (pas toujours disponible)**
- Redis Cluster en mode strict
- HBase, Zookeeper
- Préférer quand : données financières, stock

**AP — Disponibilité + Partition tolerance (éventuellement cohérent)**
- Cassandra, CouchDB, DynamoDB
- Notre Redis (un seul noeud → pas de partition possible)
- Préférer quand : disponibilité > cohérence stricte (réseaux sociaux, cache)

**CA — Cohérence + Disponibilité (pas de tolérance aux partitions)**
- Impossible en pratique sur un réseau non fiable
- MySQL en mode single-node (fonctionne mais sans distribué)

### En pratique sur NWS Watermark

```
Redis (cache) :    AP → si Redis est down, on continue sans cache (dégradation gracieuse)
MinIO (storage) :  CP → on préfère échouer que stocker un fichier corrompu
RabbitMQ (queue) : AP → les messages persistent même si le consumer est down
```

---

<a name="scaling"></a>
## 3. Scaling vertical vs horizontal

**Scaling vertical** = ajouter des ressources à un seul serveur.

```
Serveur 1 : 4 CPU, 8 GB RAM
     ↓
Serveur 1 : 16 CPU, 64 GB RAM  ← scaling vertical

Limites :
- Le serveur le plus puissant du monde a des limites physiques
- Un seul point de défaillance
- Temps d'arrêt pour upgrader le hardware
```

**Scaling horizontal** = ajouter des serveurs.

```
Serveur 1 : 4 CPU, 8 GB RAM
     ↓
Serveur 1 + Serveur 2 + Serveur 3 : chacun 4 CPU, 8 GB RAM

Avantages :
- Théoriquement infini (ajouter des serveurs)
- Pas de temps d'arrêt
- Résilience (si 1 serveur tombe, les 2 autres continuent)

Défis :
- Besoin d'un load balancer
- État partagé complexe (sessions, cache)
- Cohérence des données
```

### Notre optimizer est stateless → easy to scale

```yaml
# docker compose scale optimizer=3
optimizer:
  build: ./optimizer
  deploy:
    replicas: 3
  # Pas d'état local → n'importe quelle instance peut traiter n'importe quelle requête
```

**Stateless = chaque requête est indépendante.** L'optimizer ne garde rien en mémoire entre les requêtes → n'importe quelle instance peut traiter n'importe quelle requête.

---

<a name="load-balancing"></a>
## 4. Load Balancing — distribuer la charge

### Les algorithmes

**Round Robin :** distribuer à tour de rôle.
```
Requête 1 → Optimizer A
Requête 2 → Optimizer B
Requête 3 → Optimizer C
Requête 4 → Optimizer A
...

Problème : une image 8K prend 1s, une image 100KB prend 50ms
→ Optimizer A peut avoir 10 jobs lourds pendant qu'Optimizer B est libre
```

**Least Connections :** envoyer au moins chargé.
```
Optimizer A : 5 connexions actives
Optimizer B : 2 connexions actives  ← choisir celui-ci
Optimizer C : 3 connexions actives

→ Mieux pour des jobs de durée variable
```

**Weighted Round Robin :** certains serveurs reçoivent plus de trafic.
```
Optimizer A (4 CPU) : poids 4
Optimizer B (2 CPU) : poids 2
→ A reçoit 2x plus de trafic que B
```

**IP Hash :** même client → même serveur (sticky sessions).
```
IP 192.168.1.1 → toujours Optimizer A
IP 192.168.1.2 → toujours Optimizer B
→ Utile si l'optimizer garde un état par client (notre cas : non)
```

### Implémentation avec nginx

```nginx
upstream optimizers {
    least_conn;                       # algorithme least connections

    server optimizer_1:3001 weight=2; # plus puissant → plus de trafic
    server optimizer_2:3001 weight=1;
    server optimizer_3:3001 weight=1;

    keepalive 32;                     # pool de connexions persistantes
}

server {
    location /optimize {
        proxy_pass         http://optimizers;
        proxy_next_upstream error timeout;  # retry si un serveur est down
        proxy_connect_timeout 3s;
        proxy_read_timeout    15s;
    }
}
```

### Health checks du load balancer

```nginx
upstream optimizers {
    server optimizer_1:3001;
    server optimizer_2:3001;

    # Retirer un serveur du pool s'il échoue 3 fois en 30s
    # Le remettre après 1 succès
}
```

---

<a name="discovery"></a>
## 5. Service Discovery — trouver les services

### Le problème

Dans un système distribué, les services démarrent et s'arrêtent dynamiquement. On ne peut pas hardcoder des IPs.

```
Hardcodé : OPTIMIZER_URL=http://192.168.1.42:3001
  → Que faire si l'optimizer est sur 192.168.1.43 après un redémarrage ?
  → Que faire si on a 3 optimizers ?
```

### Solutions

**DNS-based (Docker Compose, notre cas actuel) :**
```yaml
# Docker Compose résout "optimizer" automatiquement vers le bon conteneur
environment:
  - OPTIMIZER_URL=http://optimizer:3001
```

**Consul :**
```go
// Les services s'enregistrent dans Consul au démarrage
// Les clients interrogent Consul pour trouver les IPs actives
services, _ := consulClient.Health().Service("optimizer", "", true, nil)
optimizerURL := services[0].Service.Address
```

**Kubernetes (niveau supérieur) :**
```yaml
# Un Service Kubernetes = DNS stable + load balancing automatique
# optimizer-service → plusieurs pods optimizer
apiVersion: v1
kind: Service
metadata:
  name: optimizer
spec:
  selector:
    app: optimizer  # correspond à tous les pods avec ce label
  ports:
    - port: 3001
```

---

<a name="dlq"></a>
## 6. Dead Letter Queue — ne pas perdre les messages

### Le problème actuel

```go
// ❌ Si un job échoue trop de fois → NACK → requeue → boucle infinie
msg.Nack(false, true)  // requeue=true
time.Sleep(10 * time.Second)
// → ce job tourne en boucle pour toujours, consomme des ressources
```

**Poison pill** : un message qui fait toujours planter le consumer (image corrompue, format inconnu, bug dans le code). Sans DLQ, il bloque la queue pour toujours.

### Dead Letter Queue (DLQ)

```
Queue normale : watermark_retry
  ↓ après N NACKs ou TTL dépassé
DLQ : watermark_failed
  → stocke les messages problématiques
  → peut être analysée, rejouée manuellement, alertée
```

### Configurer RabbitMQ avec DLQ

```go
// Déclarer la DLQ d'abord
amqpChan.QueueDeclare("watermark_failed", true, false, false, false, nil)

// Déclarer la queue principale avec un lien vers la DLQ
args := amqp.Table{
    // Si un message est NACK sans requeue → aller en DLQ
    "x-dead-letter-exchange":    "",
    "x-dead-letter-routing-key": "watermark_failed",

    // TTL : message expiré après 24h → aller en DLQ
    "x-message-ttl": int64(24 * 60 * 60 * 1000),  // 24h en ms

    // Max retries : après 5 NACKs → aller en DLQ
    "x-delivery-limit": 5,
}
amqpChan.QueueDeclare("watermark_retry", true, false, false, false, args)
```

### Traitement avec compteur de tentatives

```go
func processRetryJob(msg amqp.Delivery, optimizerURL string) {
    // Compter les tentatives via le header "x-death"
    var attempts int32
    if deaths, ok := msg.Headers["x-death"].([]interface{}); ok {
        for _, d := range deaths {
            if death, ok := d.(amqp.Table); ok {
                if count, ok := death["count"].(int64); ok {
                    attempts += int32(count)
                }
            }
        }
    }

    log.Info().Int32("attempt", attempts).Msg("processing retry job")

    result, err := sendToOptimizer(...)
    if err != nil {
        if attempts >= 4 {
            // 5ème tentative → laisser aller en DLQ (NACK sans requeue)
            log.Error().Int32("attempt", attempts).Msg("max retries exceeded → DLQ")
            msg.Nack(false, false)  // false = ne pas requeue → DLQ
            return
        }
        // Pas encore max → requeue avec backoff
        wait := equalJitter(int(attempts))
        msg.Nack(false, true)
        time.Sleep(wait)
        return
    }

    msg.Ack(false)
}
```

### Worker pour monitorer la DLQ

```go
func dlqMonitor() {
    msgs, _ := amqpChan.Consume("watermark_failed", "dlq-monitor", false, false, false, false, nil)

    for msg := range msgs {
        var job RetryJob
        json.Unmarshal(msg.Body, &job)

        // Alerter (Slack, PagerDuty, email...)
        log.Error().
            Str("hash", job.Hash).
            Str("filename", job.Filename).
            Msg("job in DLQ — manual intervention required")

        // ACK pour vider la DLQ (ou stocker dans une DB pour analyse)
        msg.Ack(false)
    }
}
```

---

<a name="hashing"></a>
## 7. Consistent Hashing — distribuer sans tout casser

### Le problème du sharding naïf

```
3 noeuds Redis :  hash(key) % 3 → noeud 0, 1, ou 2

hash("photo_abc") % 3 = 1  → noeud 1
hash("photo_xyz") % 3 = 0  → noeud 0

On ajoute un 4ème noeud : hash(key) % 4

hash("photo_abc") % 4 = 3  → noeud 3  ← CHANGÉ
hash("photo_xyz") % 4 = 0  → noeud 0  ← identique

→ Presque toutes les clés changent de noeud
→ 100% de cache miss pendant plusieurs heures
→ Tempête de requêtes sur l'origine
```

### Consistent Hashing — l'anneau

```
Anneau de 0 à 2^32 (4 milliards de positions)

      0
    ┌───────────────────────────────────────┐
    │         Noeud A (position 1000)       │
    │    ↗                            ↘     │
 4294967295                          1500   │ ← clé "photo_abc" (1400) → Noeud A
    │    ↖                            ↙    │
    │         Noeud B (position 2000)       │
    │    ↗                            ↘     │
    │                                3000   │ ← clé "photo_xyz" (2800) → Noeud B
    │         Noeud C (position 3500)       │
    └───────────────────────────────────────┘

Règle : une clé va au noeud dont la position est immédiatement supérieure sur l'anneau
```

**Ajouter un noeud :** seules les clés entre le nouveau noeud et son prédécesseur migrent.

```
Avant (3 noeuds) : chaque noeud gère ~33% des clés
Ajouter noeud D entre A et B : seules les clés de la portion A→D migrent (~16%)
→ 84% du cache reste valide
```

**Virtual nodes** : pour équilibrer la charge, chaque noeud physique occupe plusieurs positions sur l'anneau.

```
Noeud A physique → virtual node A_1 (position 500), A_2 (1800), A_3 (3200)...
→ distribution plus uniforme même avec des noeuds de tailles différentes
```

### Redis Cluster — implémentation réelle

Redis Cluster utilise **16384 hash slots** (pas un anneau infini) pour sa version du consistent hashing :

```
hash_slot = CRC16(key) % 16384

Cluster à 3 noeuds :
  Noeud 1 : slots 0 à 5460
  Noeud 2 : slots 5461 à 10922
  Noeud 3 : slots 10923 à 16383

Ajouter un noeud 4 :
  Déplacer ~1/4 des slots de chaque noeud vers le noeud 4
  → seulement ~25% des clés migrent
```

---

<a name="locks"></a>
## 8. Distributed Locks — synchronisation cross-services

### Le problème

```
2 instances API reçoivent la même image en même temps :
  Instance 1 : Redis MISS → commence à traiter
  Instance 2 : Redis MISS → commence à traiter (0.5ms plus tard)

→ 2 fois l'optimizer appelé pour rien
→ 2 fois Redis.Set avec le même résultat
→ Gaspillage de ressources
```

### Redlock — algorithme de verrou distribué Redis

```go
import "github.com/go-redsync/redsync/v4"

rs := redsync.New(pool)

// Acquérir un verrou sur la clé de cache
mutex := rs.NewMutex("lock:"+cacheKey,
    redsync.WithExpiry(30*time.Second),   // verrou expire après 30s (évite les deadlocks)
    redsync.WithTries(3),                 // 3 tentatives
)

if err := mutex.LockContext(ctx); err != nil {
    // Une autre instance traite déjà cette image
    // Attendre un peu et réessayer (elle aura peut-être fini)
    time.Sleep(100 * time.Millisecond)
    cached, _, hit := getFromCache(ctx, cacheKey)
    if hit {
        return sendResponse(w, r, cached), nil
    }
}
defer mutex.Unlock()

// On est le seul à traiter cette image maintenant
result, err := sendToOptimizer(...)
```

**Note :** `singleflight` (section Cache avancé dans ROADMAP.md) est plus simple et suffisant si on n'a qu'une seule instance API. Redlock est nécessaire pour plusieurs instances.

---

<a name="saga"></a>
## 9. Saga Pattern — transactions distribuées

### Le problème

Une transaction qui touche plusieurs services ne peut pas utiliser un ACID transaction classique :

```
Upload image :
  1. Sauvegarder original → MinIO
  2. Appliquer watermark  → Optimizer
  3. Stocker résultat     → Redis
  4. Enregistrer metadata → PostgreSQL (si on en avait un)

Si l'étape 4 échoue : comment annuler les étapes 1-3 ?
→ MinIO et Redis n'ont pas de "ROLLBACK"
```

### Saga — une séquence de transactions locales

```
Saga choreography (via événements) :

ImageUploaded (MinIO OK)
  → WatermarkRequested (Optimizer)
    → WatermarkCompleted (Redis OK)
      → MetadataStored (succès total)

Si MetadataStored échoue :
  → MetadataFailed
    → ResultDeleted (Redis) ← transaction compensatoire
      → OriginalDeleted (MinIO) ← transaction compensatoire
```

**Transaction compensatoire** = l'inverse logique d'une transaction. Ce n'est pas un ROLLBACK (les changements ont eu lieu), c'est une nouvelle opération qui annule l'effet.

### Implémentation simple avec RabbitMQ

```go
// Chaque étape publie un événement de succès ou d'échec
type ImageEvent struct {
    Type     string `json:"type"`     // "uploaded", "watermarked", "stored", "failed"
    Hash     string `json:"hash"`
    Step     string `json:"step"`     // "minio", "optimizer", "redis"
    Error    string `json:"error,omitempty"`
}

// Si Redis.Set échoue → publier un événement de compensation
if err := redisClient.Set(ctx, cacheKey, result, 24*time.Hour).Err(); err != nil {
    publishEvent(ImageEvent{
        Type:  "failed",
        Hash:  cacheKey,
        Step:  "redis",
        Error: err.Error(),
    })
    // Un compensating service écoute et nettoie MinIO si nécessaire
}
```

---

<a name="cqrs"></a>
## 10. CQRS — séparer lectures et écritures

**CQRS** = Command Query Responsibility Segregation.

**Principe :** séparer les opérations qui **modifient** l'état (Commands) des opérations qui **lisent** l'état (Queries).

```
Sans CQRS (actuel) :
  POST /upload  → lit le cache ET écrit le résultat
  GET /image    → lit le cache
  GET /status   → lit le cache
  → même modèle pour tout

Avec CQRS :
  Commands : POST /upload → writes vers Redis, MinIO, RabbitMQ
  Queries  : GET /image, GET /status → reads depuis Redis (ou un replica)

→ Les lectures peuvent utiliser un cache / replica différent
→ Les écritures peuvent être async (RabbitMQ)
→ On peut scaler lectures et écritures indépendamment
```

### Sur NWS Watermark

```
Command side (écriture) :
  POST /upload
  → RabbitMQ pour le processing async
  → MinIO pour le stockage permanent
  → Redis pour le cache du résultat

Query side (lecture) :
  GET /image/{hash}  → Redis (cache L1 ou L2)
  GET /status/{hash} → Redis
  → pourrait pointer vers un Redis replica read-only
  → lectures à 0 impact sur le processing
```

---

<a name="event-sourcing"></a>
## 11. Event Sourcing — stocker les événements

### L'idée centrale

**Au lieu de stocker l'état final**, on stocke tous les événements qui ont conduit à cet état.

```
Base de données classique :
  images table : {hash: "abc", status: "done", wm_text: "NWS", created_at: ...}
  → snapshot de l'état actuel

Event Sourcing :
  events stream : [
    {type: "ImageUploaded",   hash: "abc", filename: "photo.jpg", ts: T1},
    {type: "WatermarkApplied", hash: "abc", wm_text: "NWS", wm_position: "br", ts: T2},
    {type: "ResultCached",    hash: "abc", size: 245187, ts: T3},
    {type: "ImageServed",     hash: "abc", client_ip: "...", ts: T4},
  ]
  → état actuel = rejouer tous les événements
```

### Avantages

1. **Audit log complet** — on sait exactement ce qui s'est passé et quand
2. **Time-travel debugging** — rejouer jusqu'à T2 pour voir l'état à ce moment
3. **Projections multiples** — construire différentes vues depuis les mêmes événements
4. **Replay** — si le processing a eu un bug → rejouer tous les événements avec le fix

### Implémentation avec Kafka (le backend naturel pour l'event sourcing)

```go
// Produire un événement
producer.Produce(&kafka.Message{
    TopicPartition: kafka.TopicPartition{Topic: &"watermark-events"},
    Value: json.Marshal(ImageEvent{
        Type:       "ImageUploaded",
        Hash:       cacheKey,
        Filename:   header.Filename,
        Timestamp:  time.Now(),
    }),
})

// Consumer qui construit une projection "stats par heure"
for msg := range messages {
    var event ImageEvent
    json.Unmarshal(msg.Value, &event)

    switch event.Type {
    case "ResultCached":
        statsDB.IncrBy("uploads:"+event.Timestamp.Format("2006-01-02-15"), 1)
    case "WatermarkApplied":
        statsDB.HIncrBy("positions", event.WmPosition, 1)
    }
}
```

---

<a name="watermark"></a>
## 12. Utilisation dans NWS Watermark

### Ce qui est déjà distribué ✅

```
✅ API stateless → scalable horizontalement
✅ Optimizer stateless → scalable horizontalement
✅ Redis → cache partagé entre plusieurs instances API
✅ MinIO → stockage partagé
✅ RabbitMQ → queue partagée pour les retries
```

### Ce qui manque pour passer en prod ❌

```
❌ Load Balancer (nginx) devant l'optimizer
❌ Dead Letter Queue pour les jobs qui bouclent
❌ Health checks (requis pour le LB)
❌ Graceful shutdown (requis pour le rolling deploy)
❌ Distributed lock (si > 1 instance API avec même image simultanée)
❌ Redis Cluster (si le cache dépasse la RAM d'un seul serveur)
```

### Ordre d'implémentation pour scaler

```
Phase 1 — Prêt à scaler :
  1. Health checks + graceful shutdown
  2. Dead Letter Queue RabbitMQ
  3. Monitoring (Prometheus + Grafana)

Phase 2 — Scaling horizontal :
  4. nginx load balancer devant l'optimizer
  5. docker compose scale optimizer=3
  6. Tester avec k6 load test

Phase 3 — Haute disponibilité :
  7. Redis Sentinel (failover automatique)
  8. MinIO multi-noeuds (erasure coding)
  9. RabbitMQ cluster (mirrored queues)
```

---

<a name="résumé"></a>
## 13. Résumé

### Les patterns distribués en un coup d'œil

| Pattern | Problème résolu | Quand l'utiliser |
|---|---|---|
| Load Balancing | Saturation d'un seul serveur | Dès qu'on a > 1 instance |
| Service Discovery | IPs qui changent dynamiquement | Kubernetes, cloud |
| Dead Letter Queue | Poison pills, jobs qui bouclent | Toujours avec RabbitMQ |
| Consistent Hashing | Migration de cache coûteuse | Redis Cluster, sharding |
| Distributed Lock | Race conditions cross-instances | Multiple API instances |
| Saga | Transactions multi-services | Workflows complexes |
| CQRS | Lecture/écriture différents besoins | Scale lecture >> écriture |
| Event Sourcing | Audit, replay, debugging | Systèmes financiers, compliance |

### Le théorème CAP en pratique

```
Données financières     → CP  (cohérence avant disponibilité)
Cache images            → AP  (disponibilité avant cohérence stricte)
Queue de messages       → AP  (on peut avoir des doublons, c'est OK)
```

### Règles à retenir

1. **Commencer simple** — ne pas distribuer ce qui peut être centralisé
2. **Stateless d'abord** — un service stateless se scale horizontalement sans friction
3. **Toujours une DLQ** — ne jamais laisser des messages en boucle infinie
4. **CAP conscious** — choisir délibérément CP ou AP selon le besoin
5. **Mesurer avant de distribuer** — un seul serveur bien optimisé peut aller très loin
