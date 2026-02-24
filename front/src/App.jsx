import { useState, useRef } from 'react'

function formatBytes(bytes) {
  if (bytes < 1024) return bytes + ' B'
  if (bytes < 1024 * 1024) return (bytes / 1024).toFixed(1) + ' KB'
  return (bytes / (1024 * 1024)).toFixed(2) + ' MB'
}

function formatMs(ms) {
  if (ms === null || ms === undefined) return null
  const n = parseFloat(ms)
  if (n >= 1000) return (n / 1000).toFixed(2) + ' s'
  if (n >= 1)    return n.toFixed(1) + ' ms'
  return n.toFixed(2) + ' ms'
}

function parsePipeline(headers, status) {
  const steps = []
  const h = (k) => headers.get(k)

  if (h('X-T-Read'))      steps.push({ key: 'Read',      ms: h('X-T-Read'),      status: 'ok' })
  if (h('X-T-Hash'))      steps.push({ key: 'Hash',      ms: h('X-T-Hash'),      status: 'ok' })
  if (h('X-T-Redis'))     steps.push({ key: 'Redis',     ms: h('X-T-Redis'),     status: h('X-Cache') === 'HIT' ? 'hit' : 'miss' })
  if (h('X-T-Minio'))     steps.push({ key: 'MinIO',     ms: h('X-T-Minio'),     status: 'ok' })
  if (h('X-T-Optimizer')) steps.push({ key: 'Optimizer', ms: h('X-T-Optimizer'), status: 'ok' })
  if (h('X-T-Store'))     steps.push({ key: 'Store',     ms: h('X-T-Store'),     status: 'ok' })
  if (h('X-T-Rabbit'))    steps.push({ key: 'RabbitMQ',  ms: h('X-T-Rabbit'),    status: 'rabbit' })

  return steps.length > 0 ? steps : null
}

const POSITIONS = [
  { id: 'top-left',     label: 'Haut gauche',   dot: 'top-1 left-1' },
  { id: 'top-right',    label: 'Haut droite',   dot: 'top-1 right-1' },
  { id: 'bottom-left',  label: 'Bas gauche',    dot: 'bottom-1 left-1' },
  { id: 'bottom-right', label: 'Bas droite',    dot: 'bottom-1 right-1' },
]

export default function App() {
  const [preview, setPreview]       = useState(null)
  const [result, setResult]         = useState(null)
  const [loading, setLoading]       = useState(false)
  const [dragging, setDragging]     = useState(false)
  const [stats, setStats]           = useState(null)
  const [pipeline, setPipeline]     = useState(null)
  const [sliderPos, setSliderPos]   = useState(50)
  const [wmText, setWmText]         = useState('NWS © 2026')
  const [wmPosition, setWmPosition] = useState('bottom-right')
  const inputRef  = useRef(null)
  const fileRef   = useRef(null)

  const handleFile = (file) => {
    if (!file) return
    fileRef.current = file
    setPreview(URL.createObjectURL(file))
    setResult(null)
    setStats(null)
    setPipeline(null)
    setSliderPos(50)
  }

  const handleDrop = (e) => {
    e.preventDefault()
    setDragging(false)
    handleFile(e.dataTransfer.files[0])
  }

  // Polling sur /status/{jobId} — utilisé quand l'optimizer était KO (202 fallback RabbitMQ)
  const pollStatus = (jobId, file, t0) => {
    return new Promise((resolve, reject) => {
      const interval = setInterval(async () => {
        try {
          const res = await fetch(`http://localhost:3000/status/${jobId}`)
          const { status, url } = await res.json()
          if (status === 'done') {
            clearInterval(interval)
            const imgRes = await fetch(`http://localhost:3000${url}`)
            const blob = await imgRes.blob()
            const elapsed = Math.round(performance.now() - t0)
            setResult(URL.createObjectURL(blob))
            setStats({
              originalName: file.name,
              originalSize: file.size,
              processedSize: blob.size,
              ratio: (((file.size - blob.size) / file.size) * 100).toFixed(1),
              elapsed,
              cached: false,
              retried: true,
            })
            resolve()
          }
        } catch (err) {
          clearInterval(interval)
          reject(err)
        }
      }, 500)
    })
  }

  const handleUpload = async () => {
    const file = fileRef.current
    if (!file) return

    const formData = new FormData()
    formData.append('image', file)
    formData.append('wm_text', wmText)
    formData.append('wm_position', wmPosition)

    setLoading(true)
    const t0 = performance.now()
    try {
      const res = await fetch('http://localhost:3000/upload', {
        method: 'POST',
        body: formData,
      })

      // Chemin nominal (200) : optimizer OK, réponse directe
      if (res.status === 200) {
        const pipe = parsePipeline(res.headers)
        const blob = await res.blob()
        const elapsed = Math.round(performance.now() - t0)
        const cached = res.headers.get('X-Cache') === 'HIT'
        setResult(URL.createObjectURL(blob))
        setPipeline(pipe)
        setStats({
          originalName: file.name,
          originalSize: file.size,
          processedSize: blob.size,
          ratio: (((file.size - blob.size) / file.size) * 100).toFixed(1),
          elapsed,
          cached,
        })
        return
      }

      // Fallback RabbitMQ (202) : optimizer KO, job en queue → polling
      if (res.status === 202) {
        const pipe = parsePipeline(res.headers)
        const { jobId } = await res.json()
        setPipeline(pipe)
        await pollStatus(jobId, file, t0)
        return
      }

      console.error('Erreur inattendue:', res.status)
    } catch (err) {
      console.error(err)
    } finally {
      setLoading(false)
    }
  }

  const handleDownload = () => {
    const a = document.createElement('a')
    a.href = result
    a.download = 'watermarked.jpg'
    a.click()
  }

  return (
    <div className="min-h-screen bg-gray-950 text-white flex flex-col items-center justify-center p-8">
      <h1 className="text-3xl font-bold mb-2">NWS Watermark</h1>
      <p className="text-gray-400 mb-8">Dépose une image pour lui appliquer un watermark</p>

      {/* Drop zone */}
      <div
        onClick={() => inputRef.current.click()}
        onDrop={handleDrop}
        onDragOver={(e) => { e.preventDefault(); setDragging(true) }}
        onDragLeave={() => setDragging(false)}
        className={`w-full max-w-lg border-2 border-dashed rounded-2xl p-12 flex flex-col items-center justify-center cursor-pointer transition-colors
          ${dragging ? 'border-blue-400 bg-blue-950' : 'border-gray-600 hover:border-gray-400 bg-gray-900'}`}
      >
        <svg className="w-12 h-12 text-gray-500 mb-4" fill="none" stroke="currentColor" viewBox="0 0 24 24">
          <path strokeLinecap="round" strokeLinejoin="round" strokeWidth={1.5}
            d="M3 16.5v2.25A2.25 2.25 0 005.25 21h13.5A2.25 2.25 0 0021 18.75V16.5m-13.5-9L12 3m0 0l4.5 4.5M12 3v13.5" />
        </svg>
        <p className="text-gray-400">Glisse une image ici ou <span className="text-blue-400 underline">clique pour choisir</span></p>
        <p className="text-gray-600 text-sm mt-1">PNG, JPG supportés</p>
        <input ref={inputRef} type="file" accept="image/*" className="hidden"
          onChange={(e) => handleFile(e.target.files[0])} />
      </div>

      {/* Paramètres watermark */}
      <div className="mt-6 w-full max-w-lg flex flex-col gap-4">
        {/* Texte */}
        <div>
          <label className="text-xs text-gray-400 uppercase tracking-wider mb-1 block">Texte du watermark</label>
          <input
            type="text"
            value={wmText}
            onChange={(e) => setWmText(e.target.value)}
            placeholder="NWS © 2026"
            className="w-full bg-gray-900 border border-gray-700 rounded-xl px-4 py-2 text-sm text-white focus:outline-none focus:border-blue-500 transition-colors"
          />
        </div>

        {/* Position */}
        <div>
          <label className="text-xs text-gray-400 uppercase tracking-wider mb-1 block">Position</label>
          <div className="grid grid-cols-2 gap-2">
            {POSITIONS.map((pos) => {
              const selected = wmPosition === pos.id
              return (
                <button
                  key={pos.id}
                  onClick={() => setWmPosition(pos.id)}
                  className={`relative border rounded-xl p-3 h-16 text-xs font-medium transition-colors cursor-pointer
                    ${selected ? 'border-blue-500 bg-blue-600/20 text-blue-300' : 'border-gray-700 bg-gray-900 text-gray-400 hover:border-gray-500'}`}
                >
                  {pos.label}
                  <span className={`absolute w-2 h-2 rounded-full ${selected ? 'bg-blue-400' : 'bg-gray-600'} ${pos.dot}`} />
                </button>
              )
            })}
          </div>
        </div>
      </div>

      {/* Avant / Après slider */}
      {preview && result && (
        <div className="mt-8 w-full max-w-3xl">
          <p className="text-sm text-gray-400 mb-3 text-center">Glisse le curseur pour comparer</p>
          <div className="relative rounded-2xl overflow-hidden select-none" style={{ aspectRatio: '16/9' }}>

            {/* Image résultat (fond) */}
            <img src={result} className="absolute inset-0 w-full h-full object-cover" />

            {/* Image originale (clip gauche) */}
            <div className="absolute inset-0 overflow-hidden" style={{ width: `${sliderPos}%` }}>
              <img src={preview} className="absolute inset-0 w-full h-full object-cover"
                style={{ width: `${10000 / sliderPos}%`, maxWidth: 'none' }} />
            </div>

            {/* Ligne de séparation */}
            <div className="absolute top-0 bottom-0 w-0.5 bg-white shadow-lg pointer-events-none"
              style={{ left: `${sliderPos}%` }}>
              <div className="absolute top-1/2 -translate-x-1/2 -translate-y-1/2 w-8 h-8 bg-white rounded-full shadow-lg flex items-center justify-center">
                <svg className="w-4 h-4 text-gray-800" fill="none" stroke="currentColor" viewBox="0 0 24 24">
                  <path strokeLinecap="round" strokeLinejoin="round" strokeWidth={2} d="M8 9l-4 3 4 3M16 9l4 3-4 3" />
                </svg>
              </div>
            </div>

            {/* Labels */}
            <span className="absolute top-3 left-3 bg-black/60 text-white text-xs px-2 py-1 rounded-lg">Original</span>
            <span className="absolute top-3 right-3 bg-black/60 text-white text-xs px-2 py-1 rounded-lg">Watermark</span>

            {/* Slider invisible par dessus */}
            <input type="range" min="0" max="100" value={sliderPos}
              onChange={(e) => setSliderPos(Number(e.target.value))}
              className="absolute inset-0 w-full h-full opacity-0 cursor-ew-resize" />
          </div>
        </div>
      )}

      {/* Grille avant/après si pas encore de résultat */}
      {preview && !result && (
        <div className="mt-8 w-full max-w-3xl grid grid-cols-2 gap-6">
          <div>
            <p className="text-sm text-gray-400 mb-2">Original</p>
            <img src={preview} className="rounded-xl w-full object-cover" />
          </div>
          <div>
            <p className="text-sm text-gray-400 mb-2">Résultat</p>
            <div className="rounded-xl w-full aspect-video bg-gray-900 flex items-center justify-center text-gray-600">
              {loading ? 'Traitement...' : 'En attente'}
            </div>
          </div>
        </div>
      )}

      {/* Pipeline */}
      {pipeline && (
        <div className="mt-6 w-full max-w-3xl">
          <p className="text-xs text-gray-500 uppercase tracking-wider mb-3">Pipeline</p>
          <div className="flex items-center gap-1 flex-wrap">
            {pipeline.map((step, i) => {
              const colors = {
                hit:    'text-yellow-400 border-yellow-400/30 bg-yellow-400/5',
                miss:   'text-red-400    border-red-400/30    bg-red-400/5',
                rabbit: 'text-orange-400 border-orange-400/30 bg-orange-400/5',
                ok:     'text-green-400  border-green-400/30  bg-green-400/5',
              }
              const badges = { hit: '⚡ HIT', miss: 'MISS', rabbit: '🐇', ok: null }
              return (
                <div key={step.key} className="flex items-center gap-1">
                  <div className={`border rounded-lg px-3 py-2 text-center min-w-20 ${colors[step.status]}`}>
                    <p className="text-xs text-gray-400 mb-0.5">{step.key}</p>
                    {badges[step.status] && (
                      <p className="text-xs font-medium">{badges[step.status]}</p>
                    )}
                    <p className="text-xs font-mono">{formatMs(step.ms)}</p>
                  </div>
                  {i < pipeline.length - 1 && (
                    <span className="text-gray-600 text-xs">→</span>
                  )}
                </div>
              )
            })}
          </div>
        </div>
      )}

      {/* Stats */}
      {stats && (
        <div className="mt-6 w-full max-w-3xl grid grid-cols-2 sm:grid-cols-4 gap-3">
          <div className="bg-gray-900 rounded-xl p-4 text-center">
            <p className="text-xs text-gray-500 mb-1">Fichier</p>
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
              <span className={`ml-2 text-xs ${stats.ratio > 0 ? 'text-green-400' : 'text-red-400'}`}>
                {stats.ratio > 0 ? `-${stats.ratio}%` : `+${Math.abs(stats.ratio)}%`}
              </span>
            </p>
          </div>
          <div className="bg-gray-900 rounded-xl p-4 text-center">
            <p className="text-xs text-gray-500 mb-1">Temps</p>
            <p className="text-sm font-medium">
              {stats.elapsed} ms
              {stats.cached && <span className="ml-2 text-xs text-yellow-400">⚡ cache</span>}
              {stats.retried && <span className="ml-2 text-xs text-orange-400">🐇 rabbit</span>}
            </p>
          </div>
        </div>
      )}

      {/* Boutons */}
      {preview && (
        <div className="mt-6 flex gap-4">
          <button
            onClick={handleUpload}
            disabled={loading}
            className="px-8 py-3 bg-blue-600 hover:bg-blue-500 disabled:bg-gray-700 rounded-xl font-semibold transition-colors"
          >
            {loading ? 'Traitement...' : result ? 'Réappliquer' : 'Appliquer le watermark'}
          </button>

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
