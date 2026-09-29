// antd 6 迁移辅助：Dropdown.Button 已弃用（控制台告警），官方替代为
// Space.Compact + Dropdown + Button 组合。本组件保持旧拆分按钮语义：
// 主按钮独立点击 + 右侧箭头展开菜单（视觉与旧 Dropdown.Button 一致）。
import { Button, Dropdown, Space } from 'antd'
import type { ButtonProps, MenuProps } from 'antd'
import { ChevronDown } from 'lucide-react'
import type { ReactNode } from 'react'

export default function SplitButton({ children, onClick, menu, type, size, disabled, arrowLabel }: {
  children: ReactNode
  /** 主按钮点击（如「上传」直接选文件）。 */
  onClick?: () => void
  menu: MenuProps
  type?: ButtonProps['type']
  size?: ButtonProps['size']
  disabled?: boolean
  /** 箭头按钮可访问名（如「更多打开方式」）。 */
  arrowLabel?: string
}) {
  return (
    <Space.Compact>
      <Button size={size} type={type} disabled={disabled} onClick={onClick}>{children}</Button>
      <Dropdown menu={menu} placement="bottomRight" disabled={disabled}>
        <Button
          size={size}
          type={type}
          disabled={disabled}
          aria-label={arrowLabel}
          icon={<ChevronDown size={12} strokeWidth={2.2} aria-hidden="true" />}
        />
      </Dropdown>
    </Space.Compact>
  )
}
