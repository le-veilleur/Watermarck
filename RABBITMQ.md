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
┌─────────────┐    publish     ┌──────────┐   route   ┌───────────┐
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

```python
# Producteur
channel.basic_publish(
    exchange='orders',
    routing_key='order.paid',  # clé exacte
    body=json.dumps(order)
)

# Consommateur (lié à la routing key "order.paid")
channel.queue_bind(queue='payment', exchange='orders', routing_key='order.paid')
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

```python
# Producteur (routing_key ignorée)
channel.basic_publish(
    exchange='notifications',
    routing_key='',  # ignoré en fanout
    body=json.dumps(event)
)
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

```python
channel.queue_bind(queue='alertes',    exchange='logs', routing_key='log.error.*')
channel.queue_bind(queue='tous_logs',  exchange='logs', routing_key='log.#')
channel.queue_bind(queue='warns',      exchange='logs', routing_key='*.warn.*')
```

---

### 3d. Headers Exchange

**Règle :** Routage basé sur les **headers du message** (pas la routing key).

```python
# Producteur
channel.basic_publish(
    exchange='reports',
    routing_key='',  # ignoré
    properties=pika.BasicProperties(headers={'format': 'pdf', 'region': 'eu'}),
    body=report_data
)

# Binding : queue "pdf-eu" reçoit si format=pdf ET region=eu
channel.queue_bind(
    queue='pdf-eu',
    exchange='reports',
    arguments={'x-match': 'all', 'format': 'pdf', 'region': 'eu'}
    # x-match: 'all' = tous les headers doivent correspondre
    # x-match: 'any' = au moins un header doit correspondre
)
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

```python
channel.queue_declare(
    queue='orders',
    durable=True,      # survit au redémarrage de RabbitMQ
    exclusive=False,   # partagée entre plusieurs connexions
    auto_delete=False  # ne se supprime pas quand plus aucun consommateur
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

**C'est le mécanisme de scalabilité horizontal :** pour traiter plus vite, on ajoute des consommateurs.

---

<a name="bindings"></a>
## 5. Les Bindings — Connexions

Un binding est la **règle** qui relie un exchange à une queue. Sans binding, les messages arrivent dans l'exchange mais ne vont nulle part.

```python
channel.queue_bind(
    queue='payment-service',
    exchange='orders',
    routing_key='order.paid'
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
          ├── succès → channel.basic_ack()   → message supprimé de la queue ✅
          └── échec  → channel.basic_nack()  → message remis dans la queue 🔄
```

```python
def callback(ch, method, properties, body):
    try:
        process_order(json.loads(body))
        ch.basic_ack(delivery_tag=method.delivery_tag)   # ✅ OK, supprime le message
    except Exception as e:
        ch.basic_nack(
            delivery_tag=method.delivery_tag,
            requeue=True   # 🔄 remet dans la queue pour réessayer
        )

channel.basic_consume(queue='orders', on_message_callback=callback)
```

---

### Les 3 réponses possibles

| Réponse | Méthode | Effet |
|---------|---------|-------|
| Succès | `basic_ack` | Message supprimé de la queue |
| Échec + retry | `basic_nack(requeue=True)` | Message remis en tête de queue |
| Échec définitif | `basic_nack(requeue=False)` | Message envoyé en Dead Letter Queue |

---

### Auto-ACK vs Manuel

```python
# ❌ Auto-ACK : message supprimé dès réception (dangereux)
channel.basic_consume(queue='orders', on_message_callback=callback, auto_ack=True)

# ✅ ACK manuel : message supprimé seulement après traitement réussi
channel.basic_consume(queue='orders', on_message_callback=callback, auto_ack=False)
```

**Toujours utiliser l'ACK manuel pour les tâches critiques.**

---

<a name="confirms"></a>
## 7. Publisher Confirms — Garantie côté producteur

### 🎯 Le problème

Par défaut, `basic_publish` ne confirme pas que le message a bien été reçu par RabbitMQ. En cas de réseau instable, le message peut être perdu **avant même d'entrer dans la queue**.

---

### ✅ La solution : Publisher Confirms

RabbitMQ envoie un ACK/NACK au **producteur** pour confirmer la réception.

```python
# Activer les confirms
channel.confirm_delivery()

try:
    channel.basic_publish(
        exchange='orders',
        routing_key='order.paid',
        body=json.dumps(order),
        mandatory=True  # erreur si aucune queue n'est liée
    )
    print("✅ Message confirmé par RabbitMQ")
except pika.exceptions.UnroutableError:
    print("❌ Message non routable (aucune queue liée)")
except pika.exceptions.NackError:
    print("❌ RabbitMQ a refusé le message")
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
                  └── NACK (requeue=False)
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

**Configuration :**

```python
# Déclarer la DLQ
channel.queue_declare(queue='orders.dlq', durable=True)

# Déclarer la queue normale avec redirection vers la DLQ
channel.queue_declare(
    queue='orders',
    durable=True,
    arguments={
        'x-dead-letter-exchange': '',       # exchange par défaut
        'x-dead-letter-routing-key': 'orders.dlq',  # queue de destination
        'x-message-ttl': 30000,             # TTL optionnel : expire après 30s
        'x-max-length': 10000               # max 10 000 messages dans la queue
    }
)
```

**Dans le consommateur :**

```python
def callback(ch, method, properties, body):
    try:
        process_order(json.loads(body))
        ch.basic_ack(delivery_tag=method.delivery_tag)
    except Exception:
        # requeue=False → part en DLQ
        ch.basic_nack(delivery_tag=method.delivery_tag, requeue=False)
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

La queue **survit au redémarrage** de RabbitMQ (la définition est sauvegardée).

```python
channel.queue_declare(queue='orders', durable=True)  # ✅ queue persistante
```

#### Niveau 2 : Message persistant

Les messages sont **écrits sur disque** (pas seulement en RAM).

```python
channel.basic_publish(
    exchange='orders',
    routing_key='order.paid',
    body=json.dumps(order),
    properties=pika.BasicProperties(
        delivery_mode=2  # 1 = RAM seulement, 2 = disque (persistant)
    )
)
```

#### Niveau 3 : Queue durable + Message persistant = Zéro perte

```
Queue durable + delivery_mode=2
→ Si RabbitMQ crashe et redémarre :
  → La queue est recréée ✅
  → Les messages sont relus depuis le disque ✅
  → Le traitement reprend là où il en était ✅
```

---

### 📊 Comparaison des modes

| Queue | Message | Survit au redémarrage | Performance |
|-------|---------|----------------------|-------------|
| Non durable | `delivery_mode=1` | ❌ Tout perdu | Maximum |
| Durable | `delivery_mode=1` | Queue OK, messages perdus | Bonne |
| Durable | `delivery_mode=2` | ✅ Tout survit | Plus lente |

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

```python
channel.basic_qos(prefetch_count=1)
# RabbitMQ n'envoie le message suivant qu'après réception de l'ACK du précédent
```

```
Queue : [M1, M2, M3, M4, M5, M6]
prefetch_count=1

Consommateur A (rapide) :
  → reçoit M1 → traite (rapide) → ACK → reçoit M3 → traite → ACK → reçoit M5...

Consommateur B (lent) :
  → reçoit M2 → traite (lent)... → ACK → reçoit M4 → traite...

Résultat : A fait plus de travail car il ACK plus vite ✅
```

---

### Prefetch Count : quelle valeur choisir ?

| Valeur | Comportement | Use case |
|--------|-------------|----------|
| `0` | Illimité (défaut) | ❌ Ne jamais utiliser en prod |
| `1` | 1 message à la fois | Tâches longues et lourdes |
| `10-50` | Buffer raisonnable | Tâches rapides |
| `100+` | Gros buffer | Tâches très rapides, haut débit |

```python
# Tâche lourde (traitement image, ML...) → 1
channel.basic_qos(prefetch_count=1)

# Tâche légère (log, email...) → 10-50
channel.basic_qos(prefetch_count=20)
```

---

<a name="résumé"></a>
## 11. 📊 Résumé et cas d'usage

### Les exchanges en un coup d'œil

```
Direct  → 1 routing key exacte  → 1 queue
Fanout  → ignores routing key   → toutes les queues
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

### Architecture complète

```
┌──────────────┐
│  Producteur  │
│  (API REST)  │
└──────┬───────┘
       │ publish("order.paid")
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
       │ NACK (échec)
       ▼
┌──────────────┐
│ payment.dlq  │  ← Dead Letter Queue
└──────────────┘
```

---

### Concepts clés à retenir

#### 1. **Découplage**
Le producteur ne connaît pas les consommateurs. Il publie dans un exchange, c'est tout.

#### 2. **Durabilité = Queue durable + delivery_mode=2**
Sans ces deux options, les messages peuvent être perdus au redémarrage.

#### 3. **ACK manuel toujours**
Ne jamais utiliser `auto_ack=True` pour des tâches critiques. Le message doit rester en queue jusqu'à confirmation du traitement.

#### 4. **Prefetch = protection contre la surcharge**
Sans `basic_qos`, un consommateur lent peut recevoir tous les messages et les bloquer.

#### 5. **DLQ = filet de sécurité**
Les messages qui échouent répétitivement doivent aller en DLQ pour analyse, pas boucler indéfiniment.

---

## 📚 Pour aller plus loin

- **Management UI** : `http://localhost:15672` (guest/guest) — visualiser queues, exchanges, messages en temps réel
- **Shovel plugin** : transférer des messages entre brokers
- **Federation plugin** : distribuer RabbitMQ sur plusieurs datacenters
- **Quorum Queues** : remplacement des mirrored queues pour la haute disponibilité
- **Streams** : log persistant immuable (comme Kafka) disponible depuis RabbitMQ 3.9

---

**🎓 Fin du cours — RabbitMQ**