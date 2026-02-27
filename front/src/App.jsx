// useState pour les données réactives de l'UI, useRef pour les valeurs non-réactives (fichier, input DOM)
import { useState, useRef } from 'react'

// Même logique que formatBytes côté Go — dupliquée ici pour éviter un appel API juste pour l'affichage
function formatBytes(bytes) {
  if (bytes < 1024) return bytes + ' B'
  if (bytes < 1024 * 1024) return (bytes / 1024).toFixed(1) + ' KB'
  return (bytes / (1024 * 1024)).toFixed(2) + ' MB'
}

// Convertit les millisecondes renvoyées par les headers X-T-* en chaîne lisible
function formatMs(ms) {
  if (ms === null || ms === undefined) return null // header absent = étape ignorée (ex: MinIO sur cache HIT)
  const n = parseFloat(ms)                         // les headers arrivent en string
  if (n >= 1000) return (n / 1000).toFixed(2) + ' s'  // au-delà d'une seconde, afficher en s pour lisibilité
  if (n >= 1)    return n.toFixed(1) + ' ms'
  return n.toFixed(2) + ' ms'                          // en dessous de 1ms (ex: hash), garder 2 décimales
}

// Construit la liste des étapes du pipeline à partir des headers X-T-* exposés par l'API.
// Chaque header présent = l'étape a été exécutée ; absent = court-circuitée (ex: cache HIT saute l'optimizer).
function parsePipeline(headers) {
  const steps = []
  const h = (k) => headers.get(k) // raccourci pour éviter la répétition de headers.get(...)

  if (h('X-T-Read'))      steps.push({ key: 'Read',      ms: h('X-T-Read'),      status: 'ok' })
  if (h('X-T-Hash'))      steps.push({ key: 'Hash',      ms: h('X-T-Hash'),      status: 'ok' })
  // Redis est la seule étape avec trois états : hit (servi depuis le cache), miss (clé absente), ou absent (non exécuté)
  if (h('X-T-Redis'))     steps.push({ key: 'Redis',     ms: h('X-T-Redis'),     status: h('X-Cache') === 'HIT' ? 'hit' : 'miss' })
  if (h('X-T-Minio'))     steps.push({ key: 'MinIO',     ms: h('X-T-Minio'),     status: 'ok' })
  if (h('X-T-Optimizer')) steps.push({ key: 'Optimizer', ms: h('X-T-Optimizer'), status: 'ok' })
  if (h('X-T-Store'))     steps.push({ key: 'Store',     ms: h('X-T-Store'),     status: 'ok' })
  // X-T-Rabbit présent = l'optimizer était KO, le job est en queue → statut spécial orange
  if (h('X-T-Rabbit'))    steps.push({ key: 'RabbitMQ',  ms: h('X-T-Rabbit'),    status: 'rabbit' })

  return steps.length > 0 ? steps : null // null = pas de headers de timing = réponse sans pipeline (erreur)
}

// Les 4 positions correspondent aux coins de l'image.
// `dot` est une classe Tailwind qui positionne le point indicateur dans le bouton de sélection.
const POSITIONS = [
  { id: 'top-left',     label: 'Haut gauche',   dot: 'top-1 left-1' },
  { id: 'top-right',    label: 'Haut droite',   dot: 'top-1 right-1' },
  { id: 'bottom-left',  label: 'Bas gauche',    dot: 'bottom-1 left-1' },
  { id: 'bottom-right', label: 'Bas droite',    dot: 'bottom-1 right-1' },
]

export default function App() {
  const [preview, setPreview]       = useState(null)  // URL.createObjectURL du fichier local — affiché avant l'upload
  const [result, setResult]         = useState(null)  // URL.createObjectURL du blob retourné par l'API
  const [loading, setLoading]       = useState(false) // désactive le bouton et affiche "Traitement..." pendant l'appel API
  const [dragging, setDragging]     = useState(false) // change le style de la drop zone quand un fichier est survolé
  const [stats, setStats]           = useState(null)  // métriques affichées sous le slider (taille, ratio, temps)
  const [pipeline, setPipeline]     = useState(null)  // étapes du pipeline extraites des headers X-T-*
  const [sliderPos, setSliderPos]   = useState(50)    // position du curseur avant/après en % (0-100)
  const [wmText, setWmText]         = useState('NWS © 2026')    // texte du watermark envoyé comme champ wm_text
  const [wmPosition, setWmPosition] = useState('bottom-right')  // position envoyée comme champ wm_position
  const inputRef  = useRef(null)  // référence sur l'<input type="file"> caché pour déclencher le file picker au clic
  const fileRef   = useRef(null)  // stocke le File sélectionné sans re-render — évité via useState car pas affiché directement

  // Point d'entrée commun pour le drop et le file picker — évite la duplication de logique
  const handleFile = (file) => {
    if (!file) return
    fileRef.current = file                  // stocker le File pour l'envoyer dans handleUpload
    setPreview(URL.createObjectURL(file))   // prévisualisation locale — pas d'upload à ce stade
    setResult(null)                         // effacer le résultat précédent si on change d'image
    setStats(null)                          // effacer les métriques de l'image précédente
    setPipeline(null)                       // effacer le pipeline de l'image précédente
    setSliderPos(50)                        // recentrer le curseur du slider pour la nouvelle image
  }

  const handleDrop = (e) => {
    e.preventDefault()               // empêche le navigateur d'ouvrir le fichier dans un nouvel onglet
    setDragging(false)               // reset l'état de survol quand le fichier est déposé
    handleFile(e.dataTransfer.files[0]) // prendre uniquement le premier fichier (pas de multi-upload)
  }

  // Polling sur /status/{jobId} — utilisé quand l'optimizer était KO (202 fallback RabbitMQ).
  // Retourne une Promise pour pouvoir utiliser await dans handleUpload.
  const pollStatus = (jobId, file, t0) => {
    return new Promise((resolve, reject) => {
      const interval = setInterval(async () => { // toutes les 500ms — assez fréquent pour une bonne UX sans surcharger l'API
        try {
          const res = await fetch(`http://localhost:3000/status/${jobId}`)
          const { status, url } = await res.json()
          if (status === 'done') {
            clearInterval(interval) // arrêter le polling dès que l'image est disponible
            const imgRes = await fetch(`http://localhost:3000${url}`, {
              headers: { Accept: 'image/webp,image/jpeg,*/*' }, // négocier WebP comme pour l'upload initial
            })
            const blob = await imgRes.blob()
            const elapsed = Math.round(performance.now() - t0) // temps total depuis le premier clic, polling inclus
            setResult(URL.createObjectURL(blob))
            setStats({
              originalName: file.name,
              originalSize: file.size,
              processedSize: blob.size,
              ratio: (((file.size - blob.size) / file.size) * 100).toFixed(1),
              elapsed,
              cached: false,  // un job retraité via RabbitMQ n'est jamais un cache HIT
              retried: true,  // marquer pour afficher l'icône 🐇 dans les stats
            })
            resolve()
          }
          // status === 'pending' → on laisse l'interval tourner
        } catch (err) {
          clearInterval(interval) // arrêter le polling si le réseau est coupé
          reject(err)
        }
      }, 500)
    })
  }

  const handleUpload = async () => {
    const file = fileRef.current
    if (!file) return

    const formData = new FormData()
    formData.append('image', file)          // champ attendu par r.FormFile("image") côté Go
    formData.append('wm_text', wmText)      // texte du watermark
    formData.append('wm_position', wmPosition) // position parmi top-left/top-right/bottom-left/bottom-right

    setLoading(true)
    const t0 = performance.now() // démarrer le chrono côté client — inclut réseau + traitement
    try {
      const res = await fetch('http://localhost:3000/upload', {
        method: 'POST',
        // Accept: image/webp déclenche la négociation de format côté API (bestFormat) — pas de Content-Type car multipart géré par le browser
        headers: { Accept: 'image/webp,image/jpeg,*/*' },
        body: formData,
      })

      // Chemin nominal (200) : optimizer OK, image retournée directement dans la réponse
      if (res.status === 200) {
        const pipe = parsePipeline(res.headers)  // extraire le pipeline avant de consommer le body
        const blob = await res.blob()            // consommer le body (image binaire)
        const elapsed = Math.round(performance.now() - t0)
        const cached = res.headers.get('X-Cache') === 'HIT' // HIT = servi depuis Redis, pas d'optimizer
        setResult(URL.createObjectURL(blob))
        setPipeline(pipe)
        setStats({
          originalName: file.name,
          originalSize: file.size,
          processedSize: blob.size,
          ratio: (((file.size - blob.size) / file.size) * 100).toFixed(1), // positif = compression, négatif = agrandissement (rare)
          elapsed,
          cached,
        })
        return
      }

      // Fallback RabbitMQ (202) : optimizer KO, job mis en queue → on poll /status/{jobId}
      if (res.status === 202) {
        const pipe = parsePipeline(res.headers)  // X-T-Rabbit présent dans les headers du 202
        const { jobId } = await res.json()       // jobId = hash SHA256 utilisé pour le polling
        setPipeline(pipe)
        await pollStatus(jobId, file, t0)        // bloque jusqu'à ce que l'image soit disponible
        return
      }

      console.error('Erreur inattendue:', res.status) // 400 (image invalide), 502 (RabbitMQ KO aussi)
    } catch (err) {
      console.error(err) // erreur réseau (API down)
    } finally {
      setLoading(false) // toujours débloquer le bouton, même en cas d'erreur
    }
  }

  const handleDownload = () => {
    const a = document.createElement('a') // créer un lien temporaire pour déclencher le téléchargement
    a.href = result                        // URL.createObjectURL du blob résultat
    a.download = 'watermarked.jpg'         // nom suggéré pour le fichier téléchargé
    a.click()                              // simuler le clic — pas besoin d'ajouter l'élément au DOM
  }

  return (
    <div className="min-h-screen bg-gray-950 text-white flex flex-col items-center justify-center p-8">
      <h1 className="text-3xl font-bold mb-2">NWS Watermark</h1>
      <p className="text-gray-400 mb-8">Dépose une image pour lui appliquer un watermark</p>

      {/* Drop zone — cliquable ET droppable pour maximiser l'accessibilité */}
      <div
        onClick={() => inputRef.current.click()} // déléguer le clic à l'input caché pour ouvrir le file picker
        onDrop={handleDrop}
        onDragOver={(e) => { e.preventDefault(); setDragging(true) }}  // preventDefault nécessaire pour autoriser le drop
        onDragLeave={() => setDragging(false)}
        // feedback visuel bleu pendant le drag pour indiquer que la zone est active
        className={`w-full max-w-lg border-2 border-dashed rounded-2xl p-12 flex flex-col items-center justify-center cursor-pointer transition-colors
          ${dragging ? 'border-blue-400 bg-blue-950' : 'border-gray-600 hover:border-gray-400 bg-gray-900'}`}
      >
        {/* Icône upload — SVG inline pour éviter une dépendance à une lib d'icônes */}
        <svg className="w-12 h-12 text-gray-500 mb-4" fill="none" stroke="currentColor" viewBox="0 0 24 24">
          <path strokeLinecap="round" strokeLinejoin="round" strokeWidth={1.5}
            d="M3 16.5v2.25A2.25 2.25 0 005.25 21h13.5A2.25 2.25 0 0021 18.75V16.5m-13.5-9L12 3m0 0l4.5 4.5M12 3v13.5" />
        </svg>
        <p className="text-gray-400">Glisse une image ici ou <span className="text-blue-400 underline">clique pour choisir</span></p>
        <p className="text-gray-600 text-sm mt-1">PNG, JPG supportés</p>
        {/* Input caché — le vrai sélecteur de fichier, déclenché programmatiquement via inputRef */}
        <input ref={inputRef} type="file" accept="image/*" className="hidden"
          onChange={(e) => handleFile(e.target.files[0])} />
      </div>

      {/* Paramètres watermark — affichés en permanence pour permettre de modifier avant upload */}
      <div className="mt-6 w-full max-w-lg flex flex-col gap-4">
        {/* Texte */}
        <div>
          <label className="text-xs text-gray-400 uppercase tracking-wider mb-1 block">Texte du watermark</label>
          <input
            type="text"
            value={wmText}
            onChange={(e) => setWmText(e.target.value)} // contrôlé — synchronisé avec l'état wmText
            placeholder="NWS © 2026"
            className="w-full bg-gray-900 border border-gray-700 rounded-xl px-4 py-2 text-sm text-white focus:outline-none focus:border-blue-500 transition-colors"
          />
        </div>

        {/* Position — 4 boutons visuels avec un point indicateur dans le coin correspondant */}
        <div>
          <label className="text-xs text-gray-400 uppercase tracking-wider mb-1 block">Position</label>
          <div className="grid grid-cols-2 gap-2">
            {POSITIONS.map((pos) => {
              const selected = wmPosition === pos.id // détermine le style actif/inactif
              return (
                <button
                  key={pos.id}
                  onClick={() => setWmPosition(pos.id)}
                  className={`relative border rounded-xl p-3 h-16 text-xs font-medium transition-colors cursor-pointer
                    ${selected ? 'border-blue-500 bg-blue-600/20 text-blue-300' : 'border-gray-700 bg-gray-900 text-gray-400 hover:border-gray-500'}`}
                >
                  {pos.label}
                  {/* Point coloré positionné dans le coin du bouton correspondant à la position du watermark */}
                  <span className={`absolute w-2 h-2 rounded-full ${selected ? 'bg-blue-400' : 'bg-gray-600'} ${pos.dot}`} />
                </button>
              )
            })}
          </div>
        </div>
      </div>

      {/* Slider avant/après — affiché seulement quand les deux images sont disponibles */}
      {preview && result && (
        <div className="mt-8 w-full max-w-3xl">
          <p className="text-sm text-gray-400 mb-3 text-center">Glisse le curseur pour comparer</p>
          {/* overflow-hidden + select-none : empêche la sélection de texte pendant le drag du slider */}
          <div className="relative rounded-2xl overflow-hidden select-none" style={{ aspectRatio: '16/9' }}>

            {/* Image résultat en fond — visible sur toute la largeur, masquée à gauche par le clip */}
            <img src={result} className="absolute inset-0 w-full h-full object-cover" />

            {/* Image originale clippée à gauche — la largeur du div contrôle la zone visible */}
            <div className="absolute inset-0 overflow-hidden" style={{ width: `${sliderPos}%` }}>
              {/* Compensation de la largeur pour maintenir les proportions malgré le clip du parent */}
              <img src={preview} className="absolute inset-0 w-full h-full object-cover"
                style={{ width: `${10000 / sliderPos}%`, maxWidth: 'none' }} />
            </div>

            {/* Ligne de séparation + poignée circulaire centrée sur la ligne */}
            <div className="absolute top-0 bottom-0 w-0.5 bg-white shadow-lg pointer-events-none"
              style={{ left: `${sliderPos}%` }}>
              {/* Poignée — pointer-events-none sur le parent, la poignée est purement décorative */}
              <div className="absolute top-1/2 -translate-x-1/2 -translate-y-1/2 w-8 h-8 bg-white rounded-full shadow-lg flex items-center justify-center">
                <svg className="w-4 h-4 text-gray-800" fill="none" stroke="currentColor" viewBox="0 0 24 24">
                  <path strokeLinecap="round" strokeLinejoin="round" strokeWidth={2} d="M8 9l-4 3 4 3M16 9l4 3-4 3" />
                </svg>
              </div>
            </div>

            {/* Labels superposés pour indiquer quel côté est l'original */}
            <span className="absolute top-3 left-3 bg-black/60 text-white text-xs px-2 py-1 rounded-lg">Original</span>
            <span className="absolute top-3 right-3 bg-black/60 text-white text-xs px-2 py-1 rounded-lg">Watermark</span>

            {/* Input range transparent par-dessus tout — capte le drag sur toute la surface */}
            <input type="range" min="0" max="100" value={sliderPos}
              onChange={(e) => setSliderPos(Number(e.target.value))} // Number() car e.target.value est une string
              className="absolute inset-0 w-full h-full opacity-0 cursor-ew-resize" />
          </div>
        </div>
      )}

      {/* Grille avant/après — affichée en attendant le résultat (image choisie mais pas encore uploadée) */}
      {preview && !result && (
        <div className="mt-8 w-full max-w-3xl grid grid-cols-2 gap-6">
          <div>
            <p className="text-sm text-gray-400 mb-2">Original</p>
            <img src={preview} className="rounded-xl w-full object-cover" />
          </div>
          <div>
            <p className="text-sm text-gray-400 mb-2">Résultat</p>
            {/* Placeholder de même taille que l'image — évite le layout shift quand le résultat arrive */}
            <div className="rounded-xl w-full aspect-video bg-gray-900 flex items-center justify-center text-gray-600">
              {loading ? 'Traitement...' : 'En attente'}
            </div>
          </div>
        </div>
      )}

      {/* Pipeline — visualisation des étapes et leurs durées, construite depuis les headers X-T-* */}
      {pipeline && (
        <div className="mt-6 w-full max-w-3xl">
          <p className="text-xs text-gray-500 uppercase tracking-wider mb-3">Pipeline</p>
          <div className="flex items-center gap-1 flex-wrap">
            {pipeline.map((step, i) => {
              // Couleur par statut : jaune = cache HIT, rouge = cache MISS, orange = RabbitMQ, vert = nominal
              const colors = {
                hit:    'text-yellow-400 border-yellow-400/30 bg-yellow-400/5',
                miss:   'text-red-400    border-red-400/30    bg-red-400/5',
                rabbit: 'text-orange-400 border-orange-400/30 bg-orange-400/5',
                ok:     'text-green-400  border-green-400/30  bg-green-400/5',
              }
              // Badge textuel affiché sous le nom de l'étape — null = pas de badge (étape nominale)
              const badges = { hit: '⚡ HIT', miss: 'MISS', rabbit: '🐇', ok: null }
              return (
                <div key={step.key} className="flex items-center gap-1">
                  <div className={`border rounded-lg px-3 py-2 text-center min-w-20 ${colors[step.status]}`}>
                    <p className="text-xs text-gray-400 mb-0.5">{step.key}</p>
                    {badges[step.status] && ( // n'afficher le badge que s'il est non-null
                      <p className="text-xs font-medium">{badges[step.status]}</p>
                    )}
                    <p className="text-xs font-mono">{formatMs(step.ms)}</p>
                  </div>
                  {/* Flèche de séparation — pas affichée après la dernière étape */}
                  {i < pipeline.length - 1 && (
                    <span className="text-gray-600 text-xs">→</span>
                  )}
                </div>
              )
            })}
          </div>
        </div>
      )}

      {/* Stats — grille de 4 métriques affichée après le premier résultat */}
      {stats && (
        <div className="mt-6 w-full max-w-3xl grid grid-cols-2 sm:grid-cols-4 gap-3">
          <div className="bg-gray-900 rounded-xl p-4 text-center">
            <p className="text-xs text-gray-500 mb-1">Fichier</p>
            {/* title pour afficher le nom complet au survol si tronqué */}
            <p className="text-sm font-medium truncate" title={stats.originalName}>{stats.originalName}</p>
          </div>
          <div className="bg-gray-900 rounded-xl p-4 text-center">
            <p className="text-xs text-gray-500 mb-1">Taille originale</p>
            <p className="text-sm font-medium">{formatBytes(stats.originalSize)}</p>
          </div>
          <div className="bg-gray-900 rounded-xl p-4 text-center">
            <p className="text-xs text-gray-500 mb-1">Après traitement</p>
            <p className="text-sm font-medium">
              {formatBytes(stats.processedSize)}
              {/* Couleur conditionnelle : vert si compression réelle, rouge si l'image a grossi */}
              <span className={`ml-2 text-xs ${stats.ratio > 0 ? 'text-green-400' : 'text-red-400'}`}>
                {stats.ratio > 0 ? `-${stats.ratio}%` : `+${Math.abs(stats.ratio)}%`}
              </span>
            </p>
          </div>
          <div className="bg-gray-900 rounded-xl p-4 text-center">
            <p className="text-xs text-gray-500 mb-1">Temps</p>
            <p className="text-sm font-medium">
              {stats.elapsed} ms
              {/* Badges mutuellement exclusifs : cache HIT ou retraitement RabbitMQ */}
              {stats.cached  && <span className="ml-2 text-xs text-yellow-400">⚡ cache</span>}
              {stats.retried && <span className="ml-2 text-xs text-orange-400">🐇 rabbit</span>}
            </p>
          </div>
        </div>
      )}

      {/* Boutons — affichés seulement quand une image est choisie */}
      {preview && (
        <div className="mt-6 flex gap-4">
          <button
            onClick={handleUpload}
            disabled={loading} // empêche les double-clics pendant le traitement
            className="px-8 py-3 bg-blue-600 hover:bg-blue-500 disabled:bg-gray-700 rounded-xl font-semibold transition-colors"
          >
            {/* Label dynamique selon l'état : pas encore traité / en cours / déjà traité */}
            {loading ? 'Traitement...' : result ? 'Réappliquer' : 'Appliquer le watermark'}
          </button>

          {/* Bouton téléchargement — affiché seulement quand un résultat est disponible */}
          {result && (
            <button
              onClick={handleDownload}
              className="px-8 py-3 bg-green-600 hover:bg-green-500 rounded-xl font-semibold transition-colors flex items-center gap-2"
            >
              <svg className="w-5 h-5" fill="none" stroke="currentColor" viewBox="0 0 24 24">
                <path strokeLinecap="round" strokeLinejoin="round" strokeWidth={2}
                  d="M4 16v1a3 3 0 003 3h10a3 3 0 003-3v-1m-4-4l-4 4m0 0l-4-4m4 4V4" />
              </svg>
              Télécharger
            </button>
          )}
        </div>
      )}
    </div>
  )
}
