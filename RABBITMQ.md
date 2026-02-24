# Cours : RabbitMQ
## Messagerie asynchrone et architecture événementielle

---

## 📋 Table des matières

1. [C'est quoi RabbitMQ ?](#intro)
2. [Les concepts fondamentaux](#concepts)
3. [Les Exchanges — Routage des messages](#exchanges)
4. [Les Queues — File d'attente](#queues)
5. [Les Bindings — Connexions entre exchanges et queues](#bindings)
6. [Acknowledgements — Accusés de réception](#ack)
7. [Publisher Confirms — Garantie côté producteur](#confirms)
8. [Dead Letter Queue — Gestion des erreurs](#dlq)
9. [Durabilité et persistance](#durabilite)
10. [Prefetch et QoS — Contrôle de charge](#prefetch)
11. [Résumé et cas d'usage](#résumé)
12. [Implémentation dans NWS Watermark — Option B](#implementation)

---

<a name="intro"></a>
## 1. C'est quoi RabbitMQ ?

RabbitMQ est un **message broker** — un intermédiaire qui reçoit des messages d'un service et les distribue à d'autres.

**Analogie :** C'est comme La Poste.
- Le **producteur** = celui qui envoie une lettre
- RabbitMQ = La Poste (trie et achemine)
- Le **consommateur** = celui qui reçoit la lettre

```
Producteur ──► RabbitMQ ──► Consommateur
(envoie)       (stocke)      (traite)
```

---

### 🎯 Le problème sans RabbitMQ

Sans message broker, les services se parlent **directement** :

```
Service A ──► POST /api ──► Service B
```

**Problèmes :**
- Si B est down → A reçoit une erreur, le message est perdu
- Si B est lent → A attend bloqué
- Si 1000 requêtes arrivent → B est submergé

---

### ✅ La solution avec RabbitMQ

```
Service A ──► RabbitMQ ──► Service B
              (stocke si B est down)
              (régule le débit)
              (redistribue à plusieurs B)
```

**Avantages :**
- **Découplage** : A et B ne se connaissent pas
- **Résilience** : si B est down, les messages attendent dans la queue
- **Scalabilité** : on peut ajouter plusieurs instances de B
- **Débit** : RabbitMQ absorbe les pics de charge

---

### 🆚 RabbitMQ vs Redis (pub/sub)

| Critère | RabbitMQ | Redis Pub/Sub |
|---------|----------|---------------|
| Persistance des messages | ✅ Oui | ❌ Non (fire & forget) |
| Accusé de réception | ✅ Oui (ACK) | ❌ Non |
| Routage avancé | ✅ Exchanges | ❌ Non |
| Rejeu des messages | ✅ Oui | ❌ Non |
| Use case | Tâches critiques | Notifications temps réel |

---

<a name="concepts"></a>
## 2. Les concepts fondamentaux

```
┌─────────────┐    publish     ┌──────────┐   route    ┌───────────┐
│  Producteur │ ─────────────► │ Exchange │ ─────────► │   Queue   │
│ (Publisher) │                └──────────┘            └─────┬─────┘
└─────────────┘                                              │ consume
                                                             ▼
                                                    ┌─────────────────┐
                                                    │  Consommateur   │
                                                    │  (Consumer)     │
                                                    └─────────────────┘
```

| Élément | Rôle |
|---------|------|
| **Producer** | Envoie des messages à un exchange |
| **Exchange** | Reçoit les messages et les route vers les queues selon des règles |
| **Queue** | Stocke les messages en attendant qu'un consommateur les traite |
| **Consumer** | Lit et traite les messages depuis une queue |
| **Binding** | Règle qui connecte un exchange à une queue |
| **Routing Key** | Étiquette sur le message utilisée par l'exchange pour router |

---

### 📦 Le message

Un message contient :
- **Body** : le contenu (JSON, bytes, texte...)
- **Headers** : métadonnées (content-type, priority...)
- **Routing key** : étiquette pour le routage
- **Properties** : delivery_mode, expiration, reply-to...

```json
{
  "routing_key": "order.created",
  "body": { "order_id": 42, "user": "alice", "total": 99.90 },
  "properties": {
    "content_type": "application/json",
    "delivery_mode": 2
  }
}
```

---

<a name="exchanges"></a>
## 3. Les Exchanges — Routage des messages

L'exchange est le **chef d'orchestre** : il reçoit chaque message du producteur et décide dans quelle(s) queue(s) l'envoyer.

Il existe 4 types d'exchanges.

> Les exemples utilisent `github.com/rabbitmq/amqp091-go`, le client officiel Go pour RabbitMQ.

```go
import amqp "github.com/rabbitmq/amqp091-go"

conn, _ := amqp.Dial("amqp://guest:guest@localhost:5672/")
defer conn.Close()

ch, _ := conn.Channel()
defer ch.Close()
```

---

### 3a. Direct Exchange

**Règle :** Le message est envoyé dans la queue dont la **routing key correspond exactement**.

```
Producteur envoie routing_key="order.paid"
                        │
                   ┌────▼─────┐
                   │ Exchange  │
                   │ (direct) │
                   └────┬─────┘
          ┌─────────────┼─────────────┐
    "order.paid"   "order.new"   "order.cancelled"
          ▼               ▼               ▼
   ┌──────────┐   ┌──────────┐   ┌──────────────┐
   │ Queue    │   │ Queue    │   │ Queue        │
   │ payment  │   │ notify   │   │ refund       │
   └──────────┘   └──────────┘   └──────────────┘
        ✅               ❌               ❌
```

**Use case :** Traitement de tâches spécifiques par type.

```go
// Déclarer l'exchange
ch.ExchangeDeclare("orders", "direct", true, false, false, false, nil)

// Binding : queue "payment" reçoit les messages "order.paid"
ch.QueueDeclare("payment", true, false, false, false, nil)
ch.QueueBind("payment", "order.paid", "orders", false, nil)

// Producteur : publier avec routing key exacte
body, _ := json.Marshal(order)
ch.Publish("orders", "order.paid", false, false, amqp.Publishing{
    ContentType: "application/json",
    Body:        body,
})
```

---

### 3b. Fanout Exchange

**Règle :** Le message est envoyé dans **toutes les queues** liées, peu importe la routing key.

```
Producteur envoie un message
                │
           ┌────▼─────┐
           │ Exchange  │
           │ (fanout) │
           └────┬─────┘
    ┌───────────┼───────────┐
    ▼           ▼           ▼
┌────────┐ ┌────────┐ ┌──────────┐
│ emails │ │ logs   │ │ analytics│
└────────┘ └────────┘ └──────────┘
    ✅          ✅          ✅
```

**Use case :** Notifications broadcast — un événement doit déclencher plusieurs actions en parallèle.

```go
ch.ExchangeDeclare("notifications", "fanout", true, false, false, false, nil)

// Routing key ignorée en fanout — on passe une chaîne vide
ch.Publish("notifications", "", false, false, amqp.Publishing{
    ContentType: "application/json",
    Body:        body,
})
```

**Exemple :** Un utilisateur s'inscrit → envoyer un email de bienvenue + créer un log + mettre à jour les stats, tout en même temps.

---

### 3c. Topic Exchange

**Règle :** Routage par **pattern avec wildcards** sur la routing key.

```
Wildcards :
  *  = exactement un mot
  #  = zéro ou plusieurs mots
```

```
Routing keys envoyées :
  "log.error.database"
  "log.warn.api"
  "log.info.user"

Bindings :
  "log.error.*"  ──► Queue : alertes critiques
  "log.#"        ──► Queue : tous les logs
  "*.warn.*"     ──► Queue : avertissements
```

```
"log.error.database"
        │
   ┌────▼──────┐
   │  Exchange  │
   │  (topic)  │
   └────┬──────┘
        │
   ┌────▼──────────┐  ← correspond à "log.error.*" ✅
   │ alertes       │  ← correspond à "log.#"       ✅
   └───────────────┘

   ┌───────────────┐  ← correspond à "*.warn.*"    ❌
   │ avertissements│
   └───────────────┘
```

**Use case :** Logging centralisé avec filtrage par niveau et service.

```go
ch.ExchangeDeclare("logs", "topic", true, false, false, false, nil)

ch.QueueBind("alertes",   "log.error.*", "logs", false, nil)
ch.QueueBind("tous_logs", "log.#",       "logs", false, nil)
ch.QueueBind("warns",     "*.warn.*",    "logs", false, nil)

// Publier un log d'erreur
ch.Publish("logs", "log.error.database", false, false, amqp.Publishing{
    Body: []byte(`{"msg":"connexion DB perdue"}`),
})
// → reçu par "alertes" et "tous_logs", pas par "warns"
```

---

### 3d. Headers Exchange

**Règle :** Routage basé sur les **headers du message** (pas la routing key).

```go
ch.ExchangeDeclare("reports", "headers", true, false, false, false, nil)

// Binding : queue "pdf-eu" reçoit si format=pdf ET region=eu
ch.QueueBind("pdf-eu", "", "reports", false, amqp.Table{
    "x-match": "all",   // "all" = tous les headers doivent correspondre
    "format":  "pdf",   // "any" = au moins un
    "region":  "eu",
})

// Producteur
ch.Publish("reports", "", false, false, amqp.Publishing{
    Headers:     amqp.Table{"format": "pdf", "region": "eu"},
    ContentType: "application/octet-stream",
    Body:        reportData,
})
```

**Use case :** Routage complexe basé sur plusieurs critères métier.

---

### 📊 Comparaison des exchanges

| Type | Routing | Use case typique |
|------|---------|-----------------|
| **Direct** | Clé exacte | Tâches par type (email, SMS, push) |
| **Fanout** | Tout le monde | Notifications broadcast, invalidation de cache |
| **Topic** | Pattern wildcard | Logs, événements hiérarchiques |
| **Headers** | Attributs métier | Routage multi-critères |

---

<a name="queues"></a>
## 4. Les Queues — File d'attente

La queue est le **buffer** entre le producteur et le consommateur. Les messages s'y accumulent en attendant d'être traités.

```
Queue "orders" :
┌─────┬─────┬─────┬─────┬─────┐
│ M5  │ M4  │ M3  │ M2  │ M1  │  ← Messages en attente
└─────┴─────┴─────┴─────┴─────┘
                              ▲                    ▼
                           Producteur          Consommateur
                           (ajoute à la fin)   (prend au début)
```

**FIFO :** First In, First Out — le premier message arrivé est le premier traité.

---

### Déclarer une queue

```go
q, err := ch.QueueDeclare(
    "orders", // nom
    true,     // durable : survit au redémarrage de RabbitMQ
    false,    // auto-delete : supprime si plus aucun consommateur
    false,    // exclusive : réservée à cette connexion uniquement
    false,    // no-wait
    nil,      // arguments
)
```

---

### Plusieurs consommateurs sur une queue

Si plusieurs instances du même service écoutent la même queue, RabbitMQ distribue les messages en **round-robin** :

```
Queue "orders" : [M1, M2, M3, M4, M5, M6]

Consommateur A ──► reçoit M1, M3, M5
Consommateur B ──► reçoit M2, M4, M6
```

**C'est le mécanisme de scalabilité horizontale :** pour traiter plus vite, on ajoute des consommateurs.

---

<a name="bindings"></a>
## 5. Les Bindings — Connexions

Un binding est la **règle** qui relie un exchange à une queue. Sans binding, les messages arrivent dans l'exchange mais ne vont nulle part.

```go
err := ch.QueueBind(
    "payment-service", // queue
    "order.paid",      // routing key
    "orders",          // exchange
    false,
    nil,
)
```

**Analogie :** L'exchange est un carrefour, le binding est le panneau de direction.

---

<a name="ack"></a>
## 6. Acknowledgements — Accusés de réception

### 🎯 Le problème

Sans ACK, si un consommateur reçoit un message et crashe en plein traitement, le message est **perdu**.

```
Queue → Consommateur reçoit message → CRASH → message perdu 💀
```

---

### ✅ La solution : ACK manuel

Le message reste dans la queue jusqu'à ce que le consommateur envoie un ACK.

```
Queue ──► Consommateur
          │ traitement...
          │ traitement...
          ├── succès → ch.Ack()   → message supprimé de la queue ✅
          └── échec  → ch.Nack()  → message remis dans la queue 🔄
```

```go
msgs, _ := ch.Consume(
    "orders", // queue
    "",       // consumer tag
    false,    // auto-ack : false = ACK manuel ✅
    false, false, false, nil,
)

for msg := range msgs {
    err := processOrder(msg.Body)
    if err == nil {
        msg.Ack(false)             // ✅ succès → supprime le message
    } else {
        msg.Nack(false, true)      // ❌ échec → requeue=true : remet dans la queue
    }
}
```

---

### Les 3 réponses possibles

| Réponse | Méthode Go | Effet |
|---------|------------|-------|
| Succès | `msg.Ack(false)` | Message supprimé de la queue |
| Échec + retry | `msg.Nack(false, true)` | Message remis en tête de queue |
| Échec définitif | `msg.Nack(false, false)` | Message envoyé en Dead Letter Queue |

---

### Auto-ACK vs Manuel

```go
// ❌ Auto-ACK : message supprimé dès réception (dangereux)
ch.Consume("orders", "", true, false, false, false, nil)

// ✅ ACK manuel : message supprimé seulement après traitement réussi
ch.Consume("orders", "", false, false, false, false, nil)
```

**Toujours utiliser l'ACK manuel pour les tâches critiques.**

---

<a name="confirms"></a>
## 7. Publisher Confirms — Garantie côté producteur

### 🎯 Le problème

Par défaut, `Publish` ne confirme pas que le message a bien été reçu par RabbitMQ. En cas de réseau instable, le message peut être perdu **avant même d'entrer dans la queue**.

---

### ✅ La solution : Publisher Confirms

RabbitMQ envoie un ACK/NACK au **producteur** pour confirmer la réception.

```go
// Activer le mode confirms
if err := ch.Confirm(false); err != nil {
    log.Fatal("Impossible d'activer les confirms")
}

// Canal de confirmation
confirms := ch.NotifyPublish(make(chan amqp.Confirmation, 1))

body, _ := json.Marshal(order)
ch.Publish("orders", "order.paid", true, false, amqp.Publishing{
    DeliveryMode: amqp.Persistent,
    ContentType:  "application/json",
    Body:         body,
})

// Attendre la confirmation de RabbitMQ
confirm := <-confirms
if confirm.Ack {
    log.Println("✅ Message confirmé par RabbitMQ")
} else {
    log.Println("❌ RabbitMQ a refusé le message (NACK)")
}
```

---

### Garanties de livraison

| Mode | Garantie | Performance |
|------|----------|-------------|
| Fire & forget | Aucune | Maximum |
| Publisher Confirms | Reçu par RabbitMQ | Bonne |
| Confirms + ACK consommateur | Traité avec succès | Plus lente |

---

<a name="dlq"></a>
## 8. Dead Letter Queue — Gestion des erreurs

### 🎯 Le problème

Un message peut échouer plusieurs fois. Si on le remet en queue indéfiniment, il bloque les autres messages et le consommateur tourne en boucle.

```
Message M → échec → requeue → échec → requeue → ... ♾️ boucle infinie
```

---

### ✅ La solution : Dead Letter Queue (DLQ)

Après N échecs, le message est envoyé dans une queue spéciale pour analyse.

```
Queue normale ──► Consommateur
                  │
                  └── Nack(requeue=false)
                            │
                            ▼
                  ┌─────────────────┐
                  │  Dead Letter    │
                  │  Queue (DLQ)    │  ← messages problématiques
                  └─────────────────┘
                            │
                            ▼
                  Analyse / alerte / replay manuel
```

```go
// Déclarer la DLQ
ch.QueueDeclare("orders.dlq", true, false, false, false, nil)

// Déclarer la queue normale avec redirection vers la DLQ
ch.QueueDeclare("orders", true, false, false, false, amqp.Table{
    "x-dead-letter-exchange":    "",            // exchange par défaut
    "x-dead-letter-routing-key": "orders.dlq", // queue de destination
    "x-message-ttl":             int32(30000),  // expire après 30s
    "x-max-length":              int32(10000),  // max 10 000 messages
})

// Dans le consommateur
for msg := range msgs {
    if err := processOrder(msg.Body); err != nil {
        // requeue=false → part automatiquement en DLQ
        msg.Nack(false, false)
    } else {
        msg.Ack(false)
    }
}
```

---

### Retry avec délai (pattern Retry Queue)

Pour réessayer avec un délai exponentiel :

```
Queue normale ──► échec ──► Queue retry (TTL 5s) ──► Queue normale
                                                      ──► échec ──► Queue retry (TTL 30s)
                                                                     ──► ...
                                                                     ──► DLQ (après 3 tentatives)
```

---

<a name="durabilite"></a>
## 9. Durabilité et persistance

### 🎯 Le problème

Par défaut, si RabbitMQ redémarre, **toutes les queues et messages en mémoire sont perdus**.

---

### ✅ 3 niveaux de durabilité

#### Niveau 1 : Queue durable

La queue **survit au redémarrage** de RabbitMQ (la définition est sauvegardée sur disque).

```go
ch.QueueDeclare("orders", true, false, false, false, nil)
//                         ^^^^
//                         durable = true ✅
```

#### Niveau 2 : Message persistant

Les messages sont **écrits sur disque** (pas seulement en RAM).

```go
ch.Publish("orders", "order.paid", false, false, amqp.Publishing{
    DeliveryMode: amqp.Persistent, // 1 = RAM, 2 (Persistent) = disque ✅
    ContentType:  "application/json",
    Body:         body,
})
```

#### Niveau 3 : Queue durable + Message persistant = Zéro perte

```
Queue durable + DeliveryMode=Persistent
→ Si RabbitMQ crashe et redémarre :
  → La queue est recréée ✅
  → Les messages sont relus depuis le disque ✅
  → Le traitement reprend là où il en était ✅
```

---

### 📊 Comparaison des modes

| Queue | Message | Survit au redémarrage | Performance |
|-------|---------|----------------------|-------------|
| Non durable | Transient | ❌ Tout perdu | Maximum |
| Durable | Transient | Queue OK, messages perdus | Bonne |
| Durable | Persistent | ✅ Tout survit | Plus lente |

---

<a name="prefetch"></a>
## 10. Prefetch et QoS — Contrôle de charge

### 🎯 Le problème

Par défaut, RabbitMQ envoie **tous les messages disponibles** à un consommateur dès qu'il se connecte.

```
Queue : [M1, M2, M3, ..., M1000]

Consommateur A (rapide) ──► reçoit M1...M500 en mémoire, traite M1
Consommateur B (lent)   ──► reçoit M501...M1000 en mémoire, traite M501
```

**Problème :** Si B est lent, les 500 messages sont bloqués en mémoire et attendent.

---

### ✅ La solution : Prefetch Count

On limite le nombre de messages non-ACKés qu'un consommateur peut avoir en même temps.

```go
// RabbitMQ n'envoie le message suivant qu'après réception de l'ACK du précédent
ch.Qos(
    1,     // prefetch count
    0,     // prefetch size (0 = illimité)
    false, // global (false = par consommateur)
)
```

```
Queue : [M1, M2, M3, M4, M5, M6]
prefetch_count=1

Consommateur A (rapide) :
  → reçoit M1 → traite (rapide) → Ack → reçoit M3 → traite → Ack → reçoit M5...

Consommateur B (lent) :
  → reçoit M2 → traite (lent)... → Ack → reçoit M4 → traite...

Résultat : A fait plus de travail car il Ack plus vite ✅
```

---

### Prefetch Count : quelle valeur choisir ?

| Valeur | Comportement | Use case |
|--------|-------------|----------|
| `0` | Illimité (défaut) | ❌ Ne jamais utiliser en prod |
| `1` | 1 message à la fois | Tâches longues et lourdes |
| `10-50` | Buffer raisonnable | Tâches rapides |
| `100+` | Gros buffer | Tâches très rapides, haut débit |

```go
// Tâche lourde (traitement image, ML...) → 1
ch.Qos(1, 0, false)

// Tâche légère (log, email...) → 20
ch.Qos(20, 0, false)
```

---

<a name="résumé"></a>
## 11. 📊 Résumé et cas d'usage

### Les exchanges en un coup d'œil

```
Direct  → 1 routing key exacte  → 1 queue
Fanout  → ignore routing key    → toutes les queues
Topic   → pattern "log.*.error" → queues filtrées
Headers → attributs du message  → queues filtrées
```

---

### Cas d'usage classiques

| Cas d'usage | Exchange | Pattern |
|-------------|----------|---------|
| Email de confirmation commande | Direct | `order.confirmed` → queue email |
| Notification multi-canal | Fanout | 1 event → email + SMS + push |
| Logging centralisé | Topic | `log.error.*` → alertes, `log.#` → Elasticsearch |
| Traitement de fichiers | Direct | upload → queue processing → queue done |
| Workflow e-commerce | Topic | `order.#` → analytics, `order.paid` → payment |

---

### Exemple complet en Go

```go
package main

import (
    "encoding/json"
    "log"

    amqp "github.com/rabbitmq/amqp091-go"
)

func main() {
    conn, _ := amqp.Dial("amqp://guest:guest@localhost:5672/")
    defer conn.Close()
    ch, _ := conn.Channel()
    defer ch.Close()

    // Exchange topic
    ch.ExchangeDeclare("orders", "topic", true, false, false, false, nil)

    // Queues
    ch.QueueDeclare("payment",   true, false, false, false, amqp.Table{
        "x-dead-letter-exchange":    "",
        "x-dead-letter-routing-key": "payment.dlq",
    })
    ch.QueueDeclare("analytics", true, false, false, false, nil)
    ch.QueueDeclare("payment.dlq", true, false, false, false, nil)

    // Bindings
    ch.QueueBind("payment",   "order.paid", "orders", false, nil)
    ch.QueueBind("analytics", "order.#",    "orders", false, nil)

    // Prefetch
    ch.Qos(5, 0, false)

    // Consommateur
    msgs, _ := ch.Consume("payment", "", false, false, false, false, nil)

    for msg := range msgs {
        var order map[string]any
        json.Unmarshal(msg.Body, &order)

        if err := processPayment(order); err != nil {
            log.Printf("❌ Échec : %v → DLQ", err)
            msg.Nack(false, false) // → part en DLQ
        } else {
            log.Printf("✅ Paiement traité")
            msg.Ack(false)
        }
    }
}

func processPayment(order map[string]any) error {
    // traitement...
    return nil
}
```

---

### Architecture complète

```
┌──────────────┐
│  Producteur  │
│  (API REST)  │
└──────┬───────┘
       │ Publish("order.paid")
       ▼
┌──────────────┐
│   Exchange   │ type: topic
│   "orders"   │
└──────┬───────┘
       │
  ┌────┴─────────────────────────┐
  │                              │
  ▼ "order.paid"                 ▼ "order.#"
┌──────────────┐        ┌──────────────────┐
│ Queue        │        │ Queue            │
│ "payment"    │        │ "analytics"      │
└──────┬───────┘        └──────────────────┘
       │
  ┌────┴──────────────┐
  │                   │
  ▼ prefetch=5        ▼ prefetch=5
┌──────────┐     ┌──────────┐
│ Worker 1 │     │ Worker 2 │   ← scalabilité horizontale
└──────────┘     └──────────┘
       │ Nack (échec)
       ▼
┌──────────────┐
│ payment.dlq  │  ← Dead Letter Queue
└──────────────┘
```

---

### Concepts clés à retenir

#### 1. **Découplage**
Le producteur ne connaît pas les consommateurs. Il publie dans un exchange, c'est tout.

#### 2. **Durabilité = Queue durable + DeliveryMode Persistent**
Sans ces deux options, les messages peuvent être perdus au redémarrage.

#### 3. **ACK manuel toujours**
Ne jamais utiliser `auto_ack=true`. Le message doit rester en queue jusqu'à confirmation du traitement.

#### 4. **Prefetch = protection contre la surcharge**
Sans `ch.Qos()`, un consommateur lent peut recevoir tous les messages et les bloquer.

#### 5. **DLQ = filet de sécurité**
Les messages qui échouent répétitivement doivent aller en DLQ pour analyse, pas boucler indéfiniment.

---

<a name="implementation"></a>
## 12. Implémentation dans NWS Watermark — Option B

### 🎯 Le problème

L'optimizer est un microservice HTTP qui peut être temporairement indisponible (déploiement, crash, surcharge). Dans ce cas, l'image uploadée ne doit pas être perdue et le traitement doit reprendre automatiquement dès que le service est rétabli.

---

### 🏗️ Choix architectural : Option B (HTTP sync + RabbitMQ fallback)

Deux architectures étaient possibles :

| | Option A | Option B ✅ |
|---|---|---|
| Canal principal | RabbitMQ (toujours async) | HTTP direct (synchrone) |
| Canal fallback | — | RabbitMQ (si optimizer KO) |
| Réponse au client | Toujours 202 + polling | 200 direct si OK, 202 si KO |
| Complexité front | Haute (polling systématique) | Faible (polling seulement si erreur) |
| Usage RabbitMQ | Principal | Filet de sécurité |

**Option B** est choisie car : le comportement nominal reste simple et rapide (200 direct), RabbitMQ n'intervient que sur panne, la complexité est proportionnelle au besoin réel.

---

### 🔄 Flow complet

#### Chemin nominal (optimizer disponible)

```
Front                   API                   Optimizer            Redis           MinIO
  │                      │                       │                   │               │
  │──POST /upload ───────►│                       │                   │               │
  │                      │ SHA256                │                   │               │
  │                      │──────────────────────────────────────────────────────────►│ PUT original
  │                      │──── HTTP POST /optimize ──────────────────►│               │
  │                      │◄─────────── image watermarkée ─────────────│               │
  │                      │────────────────────────────────────────────►│ Redis.Set     │
  │◄─── 200 + image ──────│                       │                   │               │
```

#### Chemin fallback (optimizer KO)

```
Front                   API                RabbitMQ          Worker             Redis           MinIO
  │                      │                    │                 │                 │               │
  │──POST /upload ───────►│                   │                 │                 │               │
  │                      │──── HTTP (erreur) ─►                 │                 │               │
  │                      │──── Publish job ───►│                 │                 │               │
  │◄─── 202 {"jobId"} ────│                   │                 │                 │               │
  │                      │                   │── Deliver job ──►│                 │               │
  │─── GET /status ───────►│                   │                 │──── MinIO.Get ──────────────────►│
  │◄─── {pending} ─────────│                   │                 │◄─── original ───────────────────│
  │                       │                   │                 │──── HTTP optimizer ─►            │
  │  (optimizer revient)  │                   │                 │◄─── image ──────────             │
  │                       │                   │                 │──── Redis.Set ───────────────────►│
  │                       │                   │                 │──── ACK ──────────►│              │
  │─── GET /status ───────►│                   │                 │                   │              │
  │◄─── {done, url} ───────│                   │                 │                   │              │
  │─── GET /image/{hash} ─►│                   │                 │                   │              │
  │◄─── image ─────────────│◄────────────────────────────────────────── Redis.Get ──►│              │
```

---

### ⚙️ Initialisation RabbitMQ dans `main()`

```go
// L'URL est injectée par Docker Compose via RABBITMQ_URL
rabbitmqURL := os.Getenv("RABBITMQ_URL")
if rabbitmqURL == "" {
    rabbitmqURL = "amqp://guest:guest@localhost:5672/"
}

// Connexion TCP + authentification
amqpConn, _ := amqp.Dial(rabbitmqURL)

// Un channel = connexion virtuelle multiplexée (légère à créer)
amqpChan, _ = amqpConn.Channel()

// Déclaration de la queue durable
// durable=true : la queue survit aux redémarrages de RabbitMQ
// auto-delete=false : la queue persiste même sans consommateur actif
amqpChan.QueueDeclare(
    "watermark_retry",
    true,  // durable
    false, // auto-delete
    false, // exclusive
    false, // no-wait
    nil,
)

// Lancement du worker en arrière-plan
go retryWorker()
```

---

### 📨 Publication dans `handleUpload()` (fallback)

```go
// RetryJob : données nécessaires pour retrouver l'image et la retraiter
type RetryJob struct {
    Hash        string `json:"hash"`         // clé Redis / SHA256 de l'image
    OriginalKey string `json:"original_key"` // chemin dans MinIO : "original/<hash>.jpg"
    Filename    string `json:"filename"`     // nom original du fichier
}

// Dans handleUpload, si l'optimizer est KO :
result, err := sendToOptimizer(optimizerURL, header.Filename, data)
if err != nil {
    job := RetryJob{
        Hash:        cacheKey,
        OriginalKey: "original/" + cacheKey + ".jpg",
        Filename:    header.Filename,
    }
    body, _ := json.Marshal(job)

    amqpChan.PublishWithContext(ctx,
        "",                // exchange vide = exchange par défaut
        "watermark_retry", // routing key = nom de la queue (direct)
        false, false,
        amqp.Publishing{
            DeliveryMode: amqp.Persistent, // message écrit sur disque dans RabbitMQ
            ContentType:  "application/json",
            Body:         body,
        },
    )

    // 202 Accepted : le traitement se fera plus tard
    w.WriteHeader(http.StatusAccepted)
    json.NewEncoder(w).Encode(map[string]string{"jobId": cacheKey})
    return
}
```

**Pourquoi `DeliveryMode: Persistent` ?** Si RabbitMQ redémarre entre la publication et la consommation, le message est relu depuis le disque. Sans ce flag, il serait perdu.

**Pourquoi l'exchange vide `""` ?** L'exchange par défaut de RabbitMQ route directement vers la queue dont le nom correspond à la routing key. C'est le pattern le plus simple pour un cas point-à-point.

---

### 🔍 Endpoint `/status/{hash}` (polling)

```go
func handleStatus(w http.ResponseWriter, r *http.Request) {
    hash := r.PathValue("hash")
    ctx  := context.Background()

    // Redis.Exists retourne 1 si la clé existe, 0 sinon
    exists, _ := redisClient.Exists(ctx, hash).Result()

    w.Header().Set("Content-Type", "application/json")
    if exists == 1 {
        // Le retryWorker a terminé : le résultat est dans Redis
        json.NewEncoder(w).Encode(map[string]string{
            "status": "done",
            "url":    "/image/" + hash,
        })
    } else {
        // Le worker traite encore (ou attend que l'optimizer revienne)
        json.NewEncoder(w).Encode(map[string]string{"status": "pending"})
    }
}
```

---

### 🔁 Worker `retryWorker()`

```go
func retryWorker() {
    // Prefetch 1 : ne recevoir qu'un message à la fois
    // → garantit qu'un message non-ACKé sera re-délivré si le worker crash
    amqpChan.Qos(1, 0, false)

    msgs, _ := amqpChan.Consume(
        "watermark_retry",
        "",    // consumer tag auto-généré
        false, // auto-ack=false → ACK manuel obligatoire
        false, false, false, nil,
    )

    for msg := range msgs {
        var job RetryJob
        if err := json.Unmarshal(msg.Body, &job); err != nil {
            // Poison pill : message invalide, on l'élimine définitivement
            msg.Ack(false)
            continue
        }

        // ① Récupérer l'original depuis MinIO
        ctx := context.Background()
        obj, err := minioClient.GetObject(ctx, minioBucket, job.OriginalKey, minio.GetObjectOptions{})
        if err != nil {
            msg.Nack(false, true) // requeue=true : sera re-délivré
            time.Sleep(5 * time.Second)
            continue
        }
        data, _ := io.ReadAll(obj)
        obj.Close()

        // ② Retenter l'optimizer
        result, err := sendToOptimizer(optimizerURL, job.Filename, data)
        if err != nil {
            msg.Nack(false, true) // requeue : l'optimizer est toujours KO
            time.Sleep(10 * time.Second)
            continue
        }

        // ③ Stocker dans Redis (même clé que le chemin nominal)
        redisClient.Set(ctx, job.Hash, result, 24*time.Hour)

        // ④ ACK : message traité avec succès, retiré de la queue
        msg.Ack(false)
    }
}
```

**Cycle de vie d'un message dans le worker :**

```
RabbitMQ deliver ──► json.Unmarshal
                          │
                    ┌─────▼─────┐
                    │MinIO.Get  │ erreur → NACK (requeue) + sleep 5s
                    └─────┬─────┘
                          │ OK
                    ┌─────▼──────────┐
                    │sendToOptimizer │ erreur → NACK (requeue) + sleep 10s
                    └─────┬──────────┘
                          │ OK
                    ┌─────▼──────┐
                    │Redis.Set   │
                    └─────┬──────┘
                          │
                      Ack(false) → message supprimé de la queue ✅
```

**Pourquoi `sleep` avant de NACK ?** Sans délai, le message est immédiatement re-délivré → boucle active qui consomme du CPU inutilement. Le sleep laisse le temps à l'optimizer de redémarrer.

---

### 🖥️ Polling côté front-end (App.jsx)

```jsx
const handleUpload = async () => {
  const res = await fetch('http://localhost:3000/upload', {
    method: 'POST', body: formData,
  })

  // Chemin nominal (200) : image directement dans la réponse
  if (res.status === 200) {
    const blob = await res.blob()
    const cached = res.headers.get('X-Cache') === 'HIT'
    setResult(URL.createObjectURL(blob))
    setStats({ ...stats, cached })
    return
  }

  // Fallback RabbitMQ (202) : polling jusqu'à ce que le worker finisse
  if (res.status === 202) {
    const { jobId } = await res.json()
    await pollStatus(jobId, file, t0)
  }
}

const pollStatus = (jobId, file, t0) =>
  new Promise((resolve, reject) => {
    const interval = setInterval(async () => {
      const { status, url } = await fetch(`/status/${jobId}`).then(r => r.json())

      if (status === 'done') {
        clearInterval(interval)
        const blob = await fetch(`http://localhost:3000${url}`).then(r => r.blob())
        setResult(URL.createObjectURL(blob))
        setStats({ elapsed: Math.round(performance.now() - t0), retried: true, ... })
        resolve()
      }
    }, 500) // interroge toutes les 500ms
  })
```

**Badge `🐇 rabbit`** dans les stats : affiché quand `stats.retried === true`, pour indiquer que le résultat vient du fallback RabbitMQ.

---

### 🐳 Configuration Docker Compose

```yaml
rabbitmq:
  image: rabbitmq:3-management-alpine
  ports:
    - "5672:5672"    # AMQP (protocole messagerie)
    - "15672:15672"  # Management UI
  environment:
    - RABBITMQ_DEFAULT_USER=guest
    - RABBITMQ_DEFAULT_PASS=guest
  healthcheck:
    test: ["CMD", "rabbitmq-diagnostics", "ping"]
    interval: 10s
    timeout: 5s
    retries: 5

api:
  environment:
    - RABBITMQ_URL=amqp://guest:guest@rabbitmq:5672/
  depends_on:
    - rabbitmq
```

**Management UI** : `http://localhost:15672` — visualiser en temps réel la queue `watermark_retry`, les messages en attente, les ACK/NACK.

---

### 🧪 Tester le fallback

```bash
# 1. Lancer tous les services
docker compose up

# 2. Uploader une image (chemin nominal → 200)
curl -F "image=@photo.jpg" http://localhost:3000/upload

# 3. Arrêter l'optimizer pour simuler une panne
docker compose stop optimizer

# 4. Uploader une image (fallback → 202 + job dans RabbitMQ)
# Le front affiche "Traitement..." et poll /status/{hash}

# 5. Redémarrer l'optimizer
docker compose start optimizer

# 6. Le worker détecte la queue, récupère l'original depuis MinIO,
#    retente l'optimizer, stocke dans Redis → ACK
# Le front affiche l'image avec le badge 🐇 rabbit
```

---

### 📊 Garanties offertes par RabbitMQ dans ce setup

| Scénario | Comportement |
|----------|-------------|
| Optimizer KO au moment de l'upload | Job publié dans RabbitMQ (202) |
| API redémarre avant que le worker traite | Message toujours dans RabbitMQ (durable + persistent) |
| RabbitMQ redémarre | Queue et messages relus depuis le disque |
| Worker crash en cours de traitement | Message re-délivré (pas d'ACK envoyé = pas supprimé) |
| Optimizer revient | Worker ACK automatiquement au prochain cycle |
| Même image uploadée deux fois | Redis HIT au second upload → 200 direct, RabbitMQ non sollicité |

---

## 📚 Pour aller plus loin

- **Management UI** : `http://localhost:15672` (guest/guest) — visualiser queues, exchanges, messages en temps réel
- **`github.com/rabbitmq/amqp091-go`** : client officiel Go
- **Shovel plugin** : transférer des messages entre brokers
- **Federation plugin** : distribuer RabbitMQ sur plusieurs datacenters
- **Quorum Queues** : remplacement des mirrored queues pour la haute disponibilité
- **Streams** : log persistant immuable (comme Kafka) disponible depuis RabbitMQ 3.9

---

**🎓 Fin du cours — RabbitMQ**
