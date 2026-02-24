import { useState, useRef } from 'react'

export default function App() {
  const [preview, setPreview] = useState(null)
  const [result, setResult] = useState(null)
  const [loading, setLoading] = useState(false)
  const [dragging, setDragging] = useState(false)
  const inputRef = useRef(null)

  const handleFile = (file) => {
    if (!file) return
    setPreview(URL.createObjectURL(file))
    setResult(null)
  }

  const handleDrop = (e) => {
    e.preventDefault()
    setDragging(false)
    handleFile(e.dataTransfer.files[0])
  }

  const handleUpload = async () => {
    const file = inputRef.current.files[0]
    if (!file) return

    const formData = new FormData()
    formData.append('image', file)

    setLoading(true)
    try {
      const res = await fetch('http://localhost:3000/upload', {
        method: 'POST',
        body: formData,
      })
      const blob = await res.blob()
      setResult(URL.createObjectURL(blob))
    } catch (err) {
      console.error(err)
    } finally {
      setLoading(false)
    }
  }

  return (
    <div className="min-h-screen bg-gray-950 text-white flex flex-col items-center justify-center p-8">
      <h1 className="text-3xl font-bold mb-2">NWS Watermark</h1>
      <p className="text-gray-400 mb-8">Dépose une image pour lui appliquer un watermark</p>

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

      {preview && (
        <div className="mt-8 w-full max-w-3xl grid grid-cols-2 gap-6">
          <div>
            <p className="text-sm text-gray-400 mb-2">Original</p>
            <img src={preview} className="rounded-xl w-full object-cover" />
          </div>
          <div>
            <p className="text-sm text-gray-400 mb-2">Résultat</p>
            {result
              ? <img src={result} className="rounded-xl w-full object-cover" />
              : <div className="rounded-xl w-full aspect-video bg-gray-900 flex items-center justify-center text-gray-600">En attente</div>
            }
          </div>
        </div>
      )}

      {preview && (
        <button
          onClick={handleUpload}
          disabled={loading}
          className="mt-6 px-8 py-3 bg-blue-600 hover:bg-blue-500 disabled:bg-gray-700 rounded-xl font-semibold transition-colors"
        >
          {loading ? 'Traitement...' : 'Appliquer le watermark'}
        </button>
      )}
    </div>
  )
}