// 配额输入（v2.6）：数值 Input + 单位 Select（1024 进制，与 formatQuota
// 输出口径一致；MiB/GiB/TiB/不限），解析为字节提交后端。
// 受控组件：value 为字节数（0 = 不限），onChange 回传字节数；
// 外部 value 变化（弹窗打开/切换目标）经 useEffect 同步显示。
import { useEffect, useState } from 'react'
import { Input, Select } from 'antd'
import { formatQuota } from './FileBrowser'

/** 单位 → 字节（1024 进制；TiB 用乘法——1<<40 超出 JS 32 位位移会溢出为 256）。 */
const UNIT_BYTES: Record<'MiB' | 'GiB' | 'TiB', number> = {
  MiB: 1 << 20,
  GiB: 1 << 30,
  TiB: 1024 * 1024 * 1024 * 1024,
}

type UnitKey = 'MiB' | 'GiB' | 'TiB' | 'none'

/** 字节数 → 展示用单位（≥1TiB 取 TiB，≥1GiB 取 GiB，否则 MiB；0 取 GiB）。 */
function pickUnit(bytes: number): 'MiB' | 'GiB' | 'TiB' {
  if (bytes >= UNIT_BYTES.TiB) return 'TiB'
  if (bytes >= UNIT_BYTES.GiB) return 'GiB'
  return 'MiB'
}

/** 字节数在指定单位下的显示值（最多 2 位小数，去尾零）。 */
function toUnitText(bytes: number, unit: 'MiB' | 'GiB' | 'TiB'): string {
  const v = bytes / UNIT_BYTES[unit]
  return String(Math.round(v * 100) / 100)
}

export default function QuotaInput({
  value,
  onChange,
  disabled,
}: {
  /** 配额字节数；0 = 不限。 */
  value: number
  /** 字节数变化回调（不限时回传 0）。 */
  onChange: (bytes: number) => void
  disabled?: boolean
}) {
  const [unit, setUnit] = useState<UnitKey>(() => (value > 0 ? pickUnit(value) : 'none'))
  const [text, setText] = useState(() => (value > 0 ? toUnitText(value, pickUnit(value)) : ''))

  // 外部 value 变化时同步输入态（弹窗打开重置 / 切换编辑目标）。
  useEffect(() => {
    if (value > 0) {
      const u = pickUnit(value)
      setUnit(u)
      setText(toUnitText(value, u))
    } else {
      setUnit('none')
      setText('')
    }
  }, [value])

  const commit = (nextUnit: UnitKey, nextText: string) => {
    if (nextUnit === 'none') {
      onChange(0)
      return
    }
    const num = Number(nextText)
    if (nextText.trim() === '' || !Number.isFinite(num) || num <= 0) return
    onChange(Math.round(num * UNIT_BYTES[nextUnit]))
  }

  const currentBytes = unit === 'none' ? 0 : Math.round((Number(text) || 0) * UNIT_BYTES[unit])

  return (
    <div className="quota-input">
      <Input
        disabled={disabled || unit === 'none'}
        value={unit === 'none' ? '' : text}
        onChange={(e) => {
          setText(e.target.value)
          commit(unit, e.target.value)
        }}
        placeholder="如：10.5"
        inputMode="decimal"
      />
      <Select
        disabled={disabled}
        value={unit}
        onChange={(u) => {
          // 切单位保持字节数值不变（有效数值时换算显示文本）。
          if (u === 'none') {
            setUnit('none')
            onChange(0)
            return
          }
          const bytes = unit !== 'none' && text.trim() !== '' && Number.isFinite(Number(text))
            ? Math.round(Number(text) * UNIT_BYTES[unit])
            : currentBytes
          setUnit(u)
          if (bytes > 0) {
            setText(toUnitText(bytes, u))
            onChange(bytes)
          }
        }}
        options={[
          { value: 'MiB', label: 'MiB' },
          { value: 'GiB', label: 'GiB' },
          { value: 'TiB', label: 'TiB' },
          { value: 'none', label: '不限' },
        ]}
      />
      {value > 0 && <span className="quota-input-bytes muted">{formatQuota(value, false)}（{value.toLocaleString('en-US')} 字节）</span>}
    </div>
  )
}
