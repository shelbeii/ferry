export interface RowAction {
  id: string
  label: string
  run: () => void
  disabled?: boolean
  title?: string
  danger?: boolean
}
