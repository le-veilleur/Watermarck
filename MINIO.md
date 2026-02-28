# MinIO — configuration & usage

MinIO stocke les **images originales** avant watermarking. C'est le filet de sécurité : si l'optimizer échoue, le retry worker récupère l'original depuis MinIO pour réessayer sans que l'utilisateur ait besoin de ré-uploader.

**Fichier principal** : `api/main.go`
**SDK** : `github.com/minio/minio-go/v7`

---

## Configuration

### Variables d'environnement

| Variable | Défaut dev | Description |
|---|---|---|
| `MINIO_ENDPOINT` | `localhost:9000` | Adresse du serveur MinIO |
| `MINIO_ROOT_USER` | `minioadmin` | Access key |
| `MINIO_ROOT_PASSWORD` | `minioadmin` | Secret key |

### Bucket

```go
const minioBucket = "watermarks"   // api/main.go:29
```

Un seul bucket pour tout le projet. Créé automatiquement au démarrage s'il n'existe pas.

### Initialisation du client (`api/main.go:94`)

```go
func initMinio() *minio.Client {
    client, err := minio.New(endpoint, &minio.Options{
        Creds:  credentials.NewStaticV4(user, password, ""),
        Secure: false,   // HTTP — trafic interne Docker uniquement
    })

    exists, err := client.BucketExists(ctx, minioBucket)
    if err != nil {
        logger.Fatal().Err(err).Msg("minio inaccessible")
    }
    if !exists {
        client.MakeBucket(ctx, minioBucket, minio.MakeBucketOptions{})
        logger.Info()...Msg("bucket minio créé")
    }
    return client
}
```

**Pourquoi `Secure: false`** : trafic interne au réseau Docker — pas besoin de TLS. À activer si MinIO est exposé publiquement.

**Pourquoi `Fatal` si inaccessible** : MinIO est critique pour le pipeline de retry. Sans lui, les jobs échoués sont perdus — on préfère crasher et alerter plutôt que démarrer silencieusement en mode dégradé.

---

## Stratégie de clés

```go
// Clé MinIO : hash de l'image seule
imgSum := sha256.Sum256(data)
originalKey := "original/" + hex.EncodeToString(imgSum[:]) + ".jpg"

// Clé Redis : hash image + texte + position + format
hashInput := append(data, []byte(wmText+"|"+wmPosition+"|"+wmFormat)...)
sum := sha256.Sum256(hashInput)
cacheKey := hex.EncodeToString(sum[:])
```

**Pourquoi hasher l'image seule pour MinIO** : si le même fichier est uploadé avec des textes différents, on ne stocke qu'une copie de l'original. La clé Redis inclut le watermark pour que chaque variante ait son propre cache.

**Structure du bucket** :
```
watermarks/
└── original/
    ├── a3f4c8...d1.jpg   ← SHA-256 du fichier image
    ├── b7e2a1...f9.jpg
    └── ...
```

---

## Opérations

### `PutObject` — sauvegarde de l'original (`api/main.go:403`)

```go
func saveOriginal(ctx context.Context, key string, data []byte) time.Duration {
    t := time.Now()
    _, err := minioClient.PutObject(ctx, minioBucket, key,
        bytes.NewReader(data), int64(len(data)),
        minio.PutObjectOptions{ContentType: "image/jpeg"},
    )
    dur := time.Since(t)
    if err != nil {
        logger.Error().Err(err).Str("step", "minio_put").Str("key", key).Msg("sauvegarde original échouée")
    }
    return dur
}
```

**Comportement en cas d'erreur** : loggée mais **non fatale** — la requête continue vers l'optimizer. L'utilisateur reçoit son image watermarkée même si MinIO est temporairement indisponible. Contrepartie : si l'optimizer échoue aussi, il n'y a pas d'original pour le retry.

**Durée exposée** : header `X-T-Minio` dans la réponse HTTP (debug/monitoring).

### `GetObject` — récupération pour retry (`api/main.go:421`)

```go
func fetchFromMinio(key string) ([]byte, error) {
    obj, err := minioClient.GetObject(context.Background(), minioBucket, key, minio.GetObjectOptions{})
    if err != nil {
        return nil, err
    }
    defer obj.Close()
    data, err := io.ReadAll(obj)
    if err != nil || len(data) == 0 {
        return nil, fmt.Errorf("lecture échouée ou fichier vide")
    }
    return data, nil
}
```

Appelé par le `retryWorker`. En cas d'erreur :

```go
msg.Nack(false, true)      // requeue dans RabbitMQ
time.Sleep(5 * time.Second)
```

---

## Intégration dans le pipeline

```
Upload image
    │
    ▼
④ saveOriginal(originalKey, data)        ← PutObject
    │
    ▼
⑤ sendToOptimizer(...)
    │
    ├── succès → Redis cache → réponse HTTP
    │
    └── échec → publish RabbitMQ {originalKey, wmText, wmPosition, wmFormat}
                    │
                    └── retryWorker
                            │
                            ▼
                        fetchFromMinio(originalKey)   ← GetObject
                            │
                            ▼
                        sendToOptimizer(...)
                            └── succès → Redis cache
```

---

## Docker Compose

```yaml
minio:
  image: minio/minio
  command: server /data --console-address ":9001"
  environment:
    MINIO_ROOT_USER: minioadmin
    MINIO_ROOT_PASSWORD: minioadmin
  ports:
    - "9000:9000"   # API S3
    - "9001:9001"   # Console web
  volumes:
    - minio_data:/data
```

Console web accessible sur `http://localhost:9001` — utile pour inspecter le bucket pendant le développement.

---

## Webhooks / notifications MinIO

Le projet **n'utilise pas** les webhooks natifs de MinIO. Les notifications d'événements passent par RabbitMQ côté applicatif, ce qui est plus simple à déboguer.

Si on voulait en ajouter (par exemple notifier un service externe à chaque upload) :

```bash
# Configurer un endpoint webhook dans MinIO
mc admin config set local notify_webhook:1 \
    endpoint="http://mon-service/webhook" \
    queue_limit="10000"

# Associer des événements PUT/DELETE au bucket
mc event add local/watermarks \
    arn:minio:sqs::1:webhook \
    --event put,delete \
    --prefix "original/"

# Vérifier la configuration
mc admin config get local notify_webhook
mc event list local/watermarks
```

Les événements disponibles : `put`, `get`, `delete`, `replica`, `ilm`, `scanner`.

---

## Points de vigilance

- **Pas de TTL** sur les objets : les originaux s'accumulent. Prévoir une politique de lifecycle (suppression après N jours) si le volume devient important.
- **Pas de versioning** : si le même hash est re-uploadé, `PutObject` écrase silencieusement — c'est voulu (idempotent).
- **`Secure: false`** : à changer si MinIO est exposé hors Docker.
- **Credentials en dur dans les defaults** : acceptables en dev, à externaliser en prod (secrets manager, variables injectées).
