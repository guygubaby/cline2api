import ky, { type Options } from 'ky'

export interface ApiResponse<T> {
  success: boolean
  data: T
  message?: string
  error?: string
}

const client = ky.create({ prefix: '/admin/api/', credentials: 'same-origin', throwHttpErrors: false })

export async function api<T>(path: string, options?: Options): Promise<ApiResponse<T>> {
  const response = await client(path, options)
  const result = await response.json<ApiResponse<T>>()
  if (response.status === 401) window.dispatchEvent(new Event('admin-auth-required'))
  if (!response.ok || !result.success) throw new Error(result.error || `HTTP ${response.status}`)
  return result
}

export const get = <T,>(path: string) => api<T>(path)
export const post = <T,>(path: string, data?: unknown) => api<T>(path, { method: 'post', json: data })

export async function download(path: string, ids?: string[]) {
  const response = await client(path, ids ? { method: 'post', json: { ids } } : undefined)
  if (response.status === 401) window.dispatchEvent(new Event('admin-auth-required'))
  if (!response.ok) throw new Error(`HTTP ${response.status}`)
  const blob = await response.blob()
  const url = URL.createObjectURL(blob)
  const link = document.createElement('a')
  link.href = url
  link.download = 'cline-accounts-export.json'
  document.body.appendChild(link)
  link.click()
  link.remove()
  URL.revokeObjectURL(url)
}
