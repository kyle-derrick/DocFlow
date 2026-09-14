// PWA 图标生成（v1.1）：零图像库依赖（node:zlib + 手写最小 PNG 编码），
// 用 SDF（有向距离场）光栅化与 public/icon.svg 相同的「云文档」几何，
// 输出 public/icon-192.png 与 public/icon-512.png。
// 运行：node scripts/gen-icons.mjs（如改动 icon.svg，请同步本文件的 LAYERS 几何）。
import { mkdirSync, writeFileSync } from 'node:fs'
import { deflateSync } from 'node:zlib'
import { dirname, resolve } from 'node:path'
import { fileURLToPath } from 'node:url'

/** 512 坐标系下的圆角矩形 SDF（cx/cy 中心，hw/hh 半宽高，r 圆角）。 */
function sdRoundRect(px, py, cx, cy, hw, hh, r) {
  const dx = Math.abs(px - cx) - (hw - r)
  const dy = Math.abs(py - cy) - (hh - r)
  const ax = Math.max(dx, 0)
  const ay = Math.max(dy, 0)
  return Math.min(Math.max(dx, dy), 0) + Math.hypot(ax, ay) - r
}

/** 圆 SDF。 */
function sdCircle(px, py, cx, cy, r) {
  return Math.hypot(px - cx, py - cy) - r
}

/** 云 SDF：三圆并集 + 底部圆角矩形（并集取各距离最小值）。 */
function sdCloud(px, py) {
  return Math.min(
    sdCircle(px, py, 226, 300, 34),
    sdCircle(px, py, 268, 282, 42),
    sdCircle(px, py, 308, 300, 30),
    sdRoundRect(px, py, 267, 316, 41, 18, 9),
  )
}

// 图层（自底向上，颜色 [r,g,b]），与 public/icon.svg 一致。
const LAYERS = [
  { sdf: (x, y) => sdRoundRect(x, y, 256, 256, 224, 224, 96), color: [15, 17, 21] }, // #0f1115 底
  { sdf: (x, y) => sdRoundRect(x, y, 256, 256, 120, 168, 24), color: [230, 232, 238] }, // #e6e8ee 文档
  { sdf: (x, y) => sdRoundRect(x, y, 256, 148, 80, 8, 8), color: [139, 147, 163] }, // #8b93a3 文字线
  { sdf: (x, y) => sdRoundRect(x, y, 236, 180, 60, 8, 8), color: [139, 147, 163] },
  { sdf: sdCloud, color: [79, 124, 255] }, // #4f7cff 云
]

/** 像素覆盖：SDF 距离换算到设备像素后按 0.5 阈值线性过渡（抗锯齿）。 */
function coverage(d, scale) {
  return Math.max(0, Math.min(1, 0.5 - d * scale))
}

/** 光栅化：逐层 back-to-front source-over 合成（straight alpha）。 */
function render(size) {
  const scale = size / 512
  const rgba = Buffer.alloc(size * size * 4)
  for (let y = 0; y < size; y++) {
    for (let x = 0; x < size; x++) {
      const gx = (x + 0.5) / scale // 像素中心映射回 512 坐标系
      const gy = (y + 0.5) / scale
      let r = 0
      let g = 0
      let b = 0
      let a = 0
      for (const layer of LAYERS) {
        const la = coverage(layer.sdf(gx, gy), scale)
        if (la <= 0) continue
        const aOut = la + a * (1 - la)
        const w = a * (1 - la)
        r = (layer.color[0] * la + r * w) / aOut
        g = (layer.color[1] * la + g * w) / aOut
        b = (layer.color[2] * la + b * w) / aOut
        a = aOut
      }
      const i = (y * size + x) * 4
      rgba[i] = Math.round(r)
      rgba[i + 1] = Math.round(g)
      rgba[i + 2] = Math.round(b)
      rgba[i + 3] = Math.round(a * 255)
    }
  }
  return rgba
}

// ---------- 最小 PNG 编码（8-bit RGBA，filter 0，zlib deflate） ----------

const CRC_TABLE = new Int32Array(256)
for (let n = 0; n < 256; n++) {
  let c = n
  for (let k = 0; k < 8; k++) c = c & 1 ? 0xedb88320 ^ (c >>> 1) : c >>> 1
  CRC_TABLE[n] = c
}

function crc32(buf) {
  let c = -1
  for (let i = 0; i < buf.length; i++) c = CRC_TABLE[(c ^ buf[i]) & 0xff] ^ (c >>> 8)
  return (c ^ -1) >>> 0
}

function chunk(type, data) {
  const out = Buffer.alloc(12 + data.length)
  out.writeUInt32BE(data.length, 0)
  out.write(type, 4, 'ascii')
  data.copy(out, 8)
  out.writeUInt32BE(crc32(out.subarray(4, 8 + data.length)), 8 + data.length)
  return out
}

function encodePng(size, rgba) {
  const stride = size * 4
  const raw = Buffer.alloc((stride + 1) * size)
  for (let y = 0; y < size; y++) {
    raw[y * (stride + 1)] = 0 // filter: None
    rgba.copy(raw, y * (stride + 1) + 1, y * stride, (y + 1) * stride)
  }
  const ihdr = Buffer.alloc(13)
  ihdr.writeUInt32BE(size, 0)
  ihdr.writeUInt32BE(size, 4)
  ihdr[8] = 8 // bit depth
  ihdr[9] = 6 // color type: RGBA
  return Buffer.concat([
    Buffer.from([0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a]),
    chunk('IHDR', ihdr),
    chunk('IDAT', deflateSync(raw, { level: 9 })),
    chunk('IEND', Buffer.alloc(0)),
  ])
}

const publicDir = resolve(dirname(fileURLToPath(import.meta.url)), '..', 'public')
mkdirSync(publicDir, { recursive: true })
for (const size of [192, 512]) {
  const file = resolve(publicDir, `icon-${size}.png`)
  writeFileSync(file, encodePng(size, render(size)))
  console.log(`generated ${file}`)
}
