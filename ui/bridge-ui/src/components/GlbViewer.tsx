import { useEffect, useRef, useState } from 'react'
import { useBridgeConfig } from '../context'
import styles from './BridgeThreeDimensionalModels.module.css'

/**
 * One .glb drawn with three.js, turned with the mouse (drag to orbit, wheel to
 * zoom, right-drag to pan). three.js is loaded the first time a viewer opens,
 * so no other page carries it.
 *
 * The file is fetched with the host's own fetch (its credentials) and parsed
 * from memory. The lights are the rig the model-making agents render their
 * contact sheets with, so the model looks here as it does on their sheets.
 * A new `url` (the page adds the file's time to it) loads the file again and
 * keeps the camera where the person left it.
 */
export function GlbViewer({ url, label }: { url: string; label: string }) {
  const { fetch: fetchFn } = useBridgeConfig()
  const containerRef = useRef<HTMLDivElement>(null)
  const viewRef = useRef<ViewerHandle | null>(null)
  const [error, setError] = useState<string | null>(null)
  const [loading, setLoading] = useState(true)
  const [turning, setTurning] = useState(false)

  // The scene lives as long as the component; each file replaces its model.
  useEffect(() => {
    const container = containerRef.current
    if (!container) return
    let disposed = false
    let handle: ViewerHandle | null = null
    void createViewer(container).then(created => {
      if (disposed) {
        created.dispose()
        return
      }
      handle = created
      viewRef.current = created
    }, cause => setError(`Could not start the 3D view: ${String(cause)}`))
    return () => {
      disposed = true
      handle?.dispose()
      viewRef.current = null
    }
  }, [])

  useEffect(() => {
    let cancelled = false
    setLoading(true)
    setError(null)
    void (async () => {
      const response = await fetchFn(url)
      if (!response.ok) throw new Error(`${response.status}: ${(await response.text()).trim()}`)
      const buffer = await response.arrayBuffer()
      // The scene may still be starting the first time.
      for (let tries = 0; !viewRef.current && tries < 100 && !cancelled; tries++) await new Promise(resolve => setTimeout(resolve, 50))
      if (cancelled) return
      if (!viewRef.current) throw new Error('the 3D view did not start')
      await viewRef.current.show(buffer)
    })().then(
      () => { if (!cancelled) setLoading(false) },
      cause => { if (!cancelled) { setError(String(cause)); setLoading(false) } },
    )
    return () => { cancelled = true }
  }, [fetchFn, url])

  useEffect(() => { viewRef.current?.setTurning(turning) }, [turning])

  return (
    <div className={styles.glbViewer}>
      <div ref={containerRef} className={styles.glbCanvas} aria-label={`3D view of ${label}`} />
      <div className={styles.glbBar}>
        <label className={styles.muted}>
          <input type="checkbox" checked={turning} onChange={event => setTurning(event.target.checked)} /> Turn
        </label>
        <button type="button" className={styles.linkButton} onClick={() => viewRef.current?.frame()}>Reset view</button>
        <a href={url} download>Download</a>
        {loading && <span className={styles.muted}>Loading…</span>}
      </div>
      {error && <pre className={styles.error}>{error}</pre>}
    </div>
  )
}

interface ViewerHandle {
  show(buffer: ArrayBuffer): Promise<void>
  frame(): void
  setTurning(turning: boolean): void
  dispose(): void
}

async function createViewer(container: HTMLDivElement): Promise<ViewerHandle> {
  const THREE = await import('three')
  const { GLTFLoader } = await import('three/addons/loaders/GLTFLoader.js')
  const { OrbitControls } = await import('three/addons/controls/OrbitControls.js')

  const renderer = new THREE.WebGLRenderer({ antialias: true, alpha: true })
  renderer.setPixelRatio(Math.min(window.devicePixelRatio, 2))
  renderer.outputColorSpace = THREE.SRGBColorSpace
  container.appendChild(renderer.domElement)

  const scene = new THREE.Scene()
  scene.add(new THREE.HemisphereLight(0xfff7eb, 0xe8d9e1, 2.06))
  const sun = new THREE.DirectionalLight(0xfff3e4, 1.23)
  sun.position.set(-3, 5, 4)
  scene.add(sun)
  const rim = new THREE.DirectionalLight(0xe8f0ff, 0.48)
  rim.position.set(2, 2, -3)
  scene.add(rim)

  const camera = new THREE.PerspectiveCamera(35, 1, 0.01, 1000)
  const controls = new OrbitControls(camera, renderer.domElement)
  controls.enableDamping = true
  controls.autoRotateSpeed = 2

  let model: import('three').Object3D | null = null
  let framed = false

  const resize = () => {
    const width = Math.max(1, container.clientWidth)
    const height = Math.max(1, container.clientHeight)
    renderer.setSize(width, height, false)
    camera.aspect = width / height
    camera.updateProjectionMatrix()
  }
  const observer = new ResizeObserver(resize)
  observer.observe(container)
  resize()

  let frameRequest = 0
  const draw = () => {
    frameRequest = requestAnimationFrame(draw)
    controls.update()
    renderer.render(scene, camera)
  }
  draw()

  const frame = () => {
    if (!model) return
    const box = new THREE.Box3().setFromObject(model)
    const size = box.getSize(new THREE.Vector3())
    const center = box.getCenter(new THREE.Vector3())
    const radius = Math.max(size.length() / 2, 0.001)
    const distance = radius / Math.sin(THREE.MathUtils.degToRad(camera.fov / 2))
    // Three-quarter view from the front (+Z), a little above.
    const direction = new THREE.Vector3(0.6, 0.35, 1).normalize()
    camera.position.copy(center).addScaledVector(direction, distance * 1.05)
    camera.near = distance / 100
    camera.far = distance * 100
    camera.updateProjectionMatrix()
    controls.target.copy(center)
    controls.update()
  }

  const disposeObject = (object: import('three').Object3D) => {
    object.traverse(child => {
      const mesh = child as import('three').Mesh
      mesh.geometry?.dispose()
      const materials = Array.isArray(mesh.material) ? mesh.material : mesh.material ? [mesh.material] : []
      for (const material of materials) {
        for (const value of Object.values(material)) {
          if (value instanceof THREE.Texture) value.dispose()
        }
        material.dispose()
      }
    })
  }

  return {
    async show(buffer) {
      const gltf = await new GLTFLoader().parseAsync(buffer, '')
      if (model) {
        scene.remove(model)
        disposeObject(model)
      }
      model = gltf.scene
      scene.add(model)
      if (!framed) {
        frame()
        framed = true
      }
    },
    frame,
    setTurning(turning) { controls.autoRotate = turning },
    dispose() {
      cancelAnimationFrame(frameRequest)
      observer.disconnect()
      controls.dispose()
      if (model) disposeObject(model)
      renderer.dispose()
      renderer.domElement.remove()
    },
  }
}
