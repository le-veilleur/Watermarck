import { useState, useRef } from 'react'

function formatBytes(bytes) {
  if (bytes < 1024) return bytes + ' B'
  if (bytes < 1024 * 1024) return (bytes / 1024).toFixed(1) + ' KB'
  return (bytes / (1024 * 1024)).toFixed(2) + ' MB'
}

export default function App() {
  const [preview, setPreview]   = useState(null)
  const [result, setResult]     = useState(null)
  const [loading, setLoading]   = useState(false)
  const [dragging, setDragging] = useState(false)
  const [stats, setStats]       = useState(null)
  const [sliderPos, setSliderPos] = useState(50)
  const inputRef  = useRef(null)
  const fileRef   = useRef(null)

  const handleFile = (file) => {
    if (!file) return
    fileRef.current = file
    setPreview(URL.createObjectURL(file))
    setResult(null)
    setStats(null)
    setSliderPos(50)
  }

  const handleDrop = (e) => {
    e.preventDefault()
    setDragging(false)
    handleFile(e.dataTransfer.files[0])
  }

  const handleUpload = async () => {
    const file = fileRef.current
    if (!file) return

    const formData = new FormData()
    formData.append('image', file)

    setLoading(true)
    const t0 = performance.now()
    try {
      const res = await fetch('http://localhost:3000/upload', {
        method: 'POST',
        body: formData,
      })
      const blob = await res.blob()
      const elapsed = Math.round(performance.now() - t0)
      const cached = res.headers.get('X-Cache') === 'HIT'

      setResult(URL.createObjectURL(blob))
      setStats({
        originalName: file.name,
        originalSize: file.size,
        processedSize: blob.size,
        ratio: (((file.size - blob.size) / file.size) * 100).toFixed(1),
        elapsed,
        cached,
      })
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
