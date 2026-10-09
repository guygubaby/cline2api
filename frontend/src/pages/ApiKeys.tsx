import { useState, type FormEvent } from 'react'
import { useQuery } from '@tanstack/react-query'
import dayjs from 'dayjs'
import { Copy, KeyRound, Pencil, Plus, RefreshCw, ShieldX } from 'lucide-react'
import { PageTitle, Section, useAdmin } from '../App'
import { get, post } from '../api'
import { Button } from '../components/ui/button'
import { Input } from '../components/ui/input'
import { Sheet, SheetContent, SheetDescription, SheetFooter, SheetHeader, SheetTitle } from '../components/ui/sheet'
import type { Model } from '../types'

type ModelRule = { modelId: string; alias: string }
type KeyRecord = {
  id: string
  name: string
  preview: string
  createdAt: string
  expiresAt?: string
  lastUsedAt?: string
  revokedAt?: string
  quotaTokens: number | null
  usedTokens: number
  requestCount: number
  modelRules: ModelRule[] | null
}
type KeyForm = { name: string; expires: string; quotaM: string; modelRules: ModelRule[] }
type Panel = 'create' | 'detail' | 'edit' | null

const emptyForm = (): KeyForm => ({ name: '', expires: '', quotaM: '', modelRules: [] })
const formatTime = (value?: string) => value ? dayjs(value).format('YYYY-MM-DD HH:mm') : '—'
const formatM = (value: number) => `${new Intl.NumberFormat(undefined, { maximumFractionDigits: 6 }).format(value / 1_000_000)}M`

function keyStatus(key: KeyRecord, t: (value: string) => string) {
  if (key.revokedAt) return t('已吊销')
  if (key.expiresAt && dayjs(key.expiresAt).isBefore(dayjs())) return t('已过期')
  if (key.quotaTokens !== null && key.usedTokens >= key.quotaTokens) return t('额度用尽')
  return t('有效')
}

function initialForm(key: KeyRecord, models: Model[]): KeyForm {
  const visibleIds = new Set(models.map(model => model.id))
  const visibleRules = key.modelRules?.filter(rule => visibleIds.has(rule.modelId))
  return {
    name: key.name,
    expires: key.expiresAt ? dayjs(key.expiresAt).format('YYYY-MM-DD') : '',
    quotaM: key.quotaTokens === null ? '' : String(key.quotaTokens / 1_000_000),
    // Keep a hidden-only allowlist from silently becoming unrestricted on edit.
    modelRules: visibleRules?.length ? visibleRules : key.modelRules ?? [],
  }
}

function keyPayload(form: KeyForm, original?: KeyRecord) {
  const expiresAt = form.expires
    ? original?.expiresAt && dayjs(original.expiresAt).format('YYYY-MM-DD') === form.expires
      ? original.expiresAt
      : new Date(`${form.expires}T23:59:59`).toISOString()
    : null
  return {
    name: form.name.trim(),
    expiresAt,
    quotaM: form.quotaM === '' ? null : Number(form.quotaM),
    modelRules: form.modelRules,
  }
}

export function ApiKeysSettingsPage() {
  const { t, run, notify } = useAdmin()
  const keysQuery = useQuery({ queryKey: ['keys'], queryFn: async () => (await get<{ keys: KeyRecord[] }>('keys')).data.keys })
  const modelsQuery = useQuery({ queryKey: ['key-models'], queryFn: async () => (await get<{ models: Model[] }>('keys/models')).data.models })
  const [panel, setPanel] = useState<Panel>(null)
  const [selectedId, setSelectedId] = useState('')
  const [form, setForm] = useState<KeyForm>(emptyForm)
  const [search, setSearch] = useState('')
  const [generated, setGenerated] = useState('')
  const [saving, setSaving] = useState(false)
  const detailsQuery = useQuery({ queryKey: ['key-detail', selectedId], enabled: !!selectedId && panel !== null, queryFn: async () => (await get<KeyRecord>(`keys/detail?id=${encodeURIComponent(selectedId)}`)).data })
  const keys = keysQuery.data || []
  const models = modelsQuery.data || []
  const filteredModels = models.filter(model => model.id.toLowerCase().includes(search.toLowerCase()))
  const selected = detailsQuery.data || keys.find(key => key.id === selectedId)

  function openCreate() { setSelectedId(''); setForm(emptyForm()); setSearch(''); setPanel('create') }
  function openDetail(key: KeyRecord) { setSelectedId(key.id); setPanel('detail') }
  function openEdit(key: KeyRecord) { setSelectedId(key.id); setForm(initialForm(key, models)); setSearch(''); setPanel('edit') }
  function toggleModel(modelId: string, checked: boolean) {
    setForm(current => ({ ...current, modelRules: checked
      ? [...current.modelRules, { modelId, alias: '' }]
      : current.modelRules.filter(rule => rule.modelId !== modelId) }))
  }
  function setAlias(modelId: string, alias: string) {
    setForm(current => ({ ...current, modelRules: current.modelRules.map(rule => rule.modelId === modelId ? { ...rule, alias } : rule) }))
  }
  function selectFilteredModels() {
    setForm(current => {
      const selectedIds = new Set(current.modelRules.map(rule => rule.modelId))
      return { ...current, modelRules: [...current.modelRules, ...filteredModels.filter(model => !selectedIds.has(model.id)).map(model => ({ modelId: model.id, alias: '' }))] }
    })
  }
  async function save(event: FormEvent<HTMLFormElement>) {
    event.preventDefault()
    if (form.quotaM !== '' && (!Number.isFinite(Number(form.quotaM)) || Number(form.quotaM) < 0.000001 || Number(form.quotaM) > 1_000_000)) {
      notify(t('额度必须大于 0 且不超过 100 万 M'), true)
      return
    }
    setSaving(true)
    const editing = panel === 'edit' && selected
    const body = keyPayload(form, editing ? selected : undefined)
    const succeeded = await run(async () => {
      if (editing) {
        await post('keys/update', { id: selected.id, ...body })
      } else {
        const response = await post<{ key: string }>('keys/generate', body)
        setGenerated(response.data.key)
      }
    }, editing ? '密钥已更新' : '密钥已生成')
    setSaving(false)
    if (succeeded) setPanel(editing ? 'detail' : null)
  }
  async function revoke(key: KeyRecord) {
    if (!confirm(`${t('确定吊销此密钥？')} ${key.name}`)) return
    if (await run(() => post('keys/delete', { id: key.id }), '密钥已吊销')) setPanel(null)
  }
  async function copyKey() {
    try { await navigator.clipboard.writeText(generated); notify(t('已复制到剪贴板')) }
    catch (error) { notify((error as Error).message, true) }
  }

  return <>
    <PageTitle title={t('API 密钥')} subtitle={t('为客户端分配模型权限和 Token 额度')} action={<div className="flex gap-2"><Button variant="outline" onClick={() => { keysQuery.refetch(); if (selectedId) detailsQuery.refetch() }}><RefreshCw aria-hidden="true"/>{t('刷新')}</Button><Button onClick={openCreate}><Plus aria-hidden="true"/>{t('创建 API 密钥')}</Button></div>}/>
    {generated && <div className="mb-5 rounded-xl border border-emerald-300 bg-emerald-50 p-4 text-emerald-950" role="status"><div className="flex items-start gap-3"><KeyRound aria-hidden="true" className="mt-0.5 size-5 shrink-0"/><div className="min-w-0 flex-1"><p className="font-semibold">{t('密钥只显示这一次，请立即复制保存')}</p><code className="mt-2 block break-all rounded-md bg-white/70 p-3 text-sm">{generated}</code><div className="mt-3 flex gap-2"><Button size="sm" onClick={copyKey}><Copy aria-hidden="true"/>{t('复制密钥')}</Button><Button size="sm" variant="outline" onClick={() => setGenerated('')}>{t('完成')}</Button></div></div></div></div>}
    <Section title={t('密钥列表')} aside={<span className="text-xs text-muted-foreground">{keys.length} {t('根密钥')}</span>}>
      {keysQuery.error && <p role="alert" className="text-sm text-destructive">{keysQuery.error.message}</p>}
      <div className="grid gap-3 lg:grid-cols-2">{keys.map(key => <article key={key.id} className="min-w-0 rounded-xl border p-4">
        <div className="flex items-start justify-between gap-3"><div className="min-w-0"><h2 className="break-words font-semibold">{key.name}</h2><p className="mt-1 font-mono text-xs text-muted-foreground">{key.preview}</p></div><span className={`shrink-0 rounded-full px-2 py-1 text-xs ${keyStatus(key, t) === t('有效') ? 'bg-emerald-50 text-emerald-700' : 'bg-amber-50 text-amber-700'}`}>{keyStatus(key, t)}</span></div>
        <dl className="mt-4 grid grid-cols-2 gap-3 text-sm"><div><dt className="text-xs text-muted-foreground">{t('使用 / 额度')}</dt><dd className="mt-1 font-medium">{formatM(key.usedTokens)} / {key.quotaTokens === null ? t('不限额') : formatM(key.quotaTokens)}</dd></div><div><dt className="text-xs text-muted-foreground">{t('可用模型')}</dt><dd className="mt-1 font-medium">{key.modelRules === null ? t('全部模型') : key.modelRules.length}</dd></div><div><dt className="text-xs text-muted-foreground">{t('到期时间')}</dt><dd className="mt-1 font-medium">{key.expiresAt ? dayjs(key.expiresAt).format('YYYY-MM-DD') : t('永不过期')}</dd></div><div><dt className="text-xs text-muted-foreground">{t('最后使用')}</dt><dd className="mt-1 font-medium">{formatTime(key.lastUsedAt)}</dd></div></dl>
        <div className="mt-4 flex flex-wrap gap-2 border-t pt-4"><Button size="sm" variant="outline" onClick={() => openDetail(key)}>{t('详情')}</Button><Button size="sm" variant="outline" disabled={!!key.revokedAt} onClick={() => openEdit(key)}>{t('编辑')}</Button><Button size="sm" variant="destructive" disabled={!!key.revokedAt} onClick={() => revoke(key)}>{t('吊销')}</Button></div>
      </article>)}{!keys.length && !keysQuery.isLoading && <p className="py-8 text-center text-sm text-muted-foreground">{t('暂无 API 密钥')}</p>}{keysQuery.isLoading && <p className="py-8 text-center text-sm text-muted-foreground">{t('加载中…')}</p>}</div>
    </Section>
    <Sheet open={panel !== null} onOpenChange={open => { if (!open) setPanel(null) }}>
      <SheetContent className="w-full max-w-full gap-0 overflow-y-auto sm:max-w-2xl!">
        <SheetHeader className="border-b px-6 py-5"><SheetTitle>{t(panel === 'create' ? '创建 API 密钥' : panel === 'edit' ? '编辑 API 密钥' : 'API 密钥详情')}</SheetTitle><SheetDescription>{t(panel === 'detail' ? '查看使用情况、模型权限和有效期' : '配置有效期、额度和允许调用的模型')}</SheetDescription></SheetHeader>
        {panel === 'detail' && selected && <div className="grid gap-6 px-6 py-5">
          <div><h3 className="text-lg font-semibold">{selected.name}</h3><p className="mt-1 font-mono text-xs text-muted-foreground">{selected.preview} · {selected.id}</p></div>
          <dl className="grid grid-cols-2 gap-4 rounded-xl border p-4 text-sm sm:grid-cols-3">{([
            [t('状态'), keyStatus(selected, t)], [t('计量请求'), new Intl.NumberFormat().format(selected.requestCount)],
            [t('已使用'), formatM(selected.usedTokens)], [t('额度'), selected.quotaTokens === null ? t('不限额') : formatM(selected.quotaTokens)],
            [t('创建时间'), formatTime(selected.createdAt)], [t('最后使用'), formatTime(selected.lastUsedAt)],
            [t('到期时间'), selected.expiresAt ? formatTime(selected.expiresAt) : t('永不过期')],
          ] as const).map(([label, value]) => <div key={label}><dt className="text-muted-foreground">{label}</dt><dd className="mt-1 font-medium">{value}</dd></div>)}</dl>
          {selected.quotaTokens !== null && <div><div className="mb-2 flex justify-between text-sm"><span>{t('额度使用进度')}</span><span>{Math.min(100, Math.round(selected.usedTokens / selected.quotaTokens * 100))}%</span></div><progress className="h-2 w-full accent-primary" max={selected.quotaTokens} value={Math.min(selected.usedTokens, selected.quotaTokens)}>{selected.usedTokens} / {selected.quotaTokens}</progress><p className="mt-1 text-xs text-muted-foreground">{t('剩余额度')}: {formatM(Math.max(0, selected.quotaTokens - selected.usedTokens))}</p></div>}
          <div><h3 className="font-semibold">{t('可用模型')} · {selected.modelRules === null ? t('全部模型') : selected.modelRules.length}</h3>{selected.modelRules === null ? <p className="mt-2 text-sm text-muted-foreground">{t('模型列表自动跟随全局模型展示配置，无需逐个勾选。')}</p> : <div className="mt-3 max-h-64 space-y-2 overflow-y-auto">{selected.modelRules.map(rule => <div key={rule.modelId} className="rounded-lg border px-3 py-2 text-sm"><code className="break-all">{rule.alias || rule.modelId}</code>{rule.alias && <span className="block text-xs text-muted-foreground">→ {rule.modelId}</span>}</div>)}</div>}</div>
        </div>}
        {panel === 'detail' && selected && <SheetFooter className="flex-row justify-end border-t px-6 py-4"><Button variant="outline" onClick={() => openEdit(selected)} disabled={!!selected.revokedAt}><Pencil aria-hidden="true"/>{t('编辑')}</Button><Button variant="destructive" onClick={() => revoke(selected)} disabled={!!selected.revokedAt}><ShieldX aria-hidden="true"/>{t('吊销')}</Button></SheetFooter>}
        {(panel === 'create' || panel === 'edit') && <form onSubmit={save} className="flex min-h-0 flex-1 flex-col"><div className="grid flex-1 content-start gap-5 px-6 py-5">
          <div className="grid gap-4 sm:grid-cols-2"><label className="grid gap-2 text-sm font-medium">{t('用途名称')}<Input name="name" required maxLength={80} value={form.name} onChange={event => setForm({ ...form, name: event.target.value })} placeholder={t('例如：生产环境客户端')}/></label><label className="grid gap-2 text-sm font-medium">{t('到期日期（可选）')}<Input name="expires" type="date" min={panel === 'create' ? dayjs().format('YYYY-MM-DD') : undefined} value={form.expires} onChange={event => setForm({ ...form, expires: event.target.value })}/></label></div>
          <label className="grid gap-2 text-sm font-medium">{t('额度上限（M Token）')}<Input name="quotaM" type="number" min="0.000001" max="1000000" step="any" inputMode="decimal" value={form.quotaM} onChange={event => setForm({ ...form, quotaM: event.target.value })} placeholder={t('留空表示不限额')}/><span className="text-xs font-normal text-muted-foreground">{t('1M = 100 万 Token；达到额度后禁止新的生成请求。')}</span></label>
          <fieldset className="min-w-0 rounded-xl border p-4"><legend className="px-1 text-sm font-semibold">{t('可用模型')} · {form.modelRules.length ? form.modelRules.length : t('全部模型')}</legend><p className="mb-3 text-xs text-muted-foreground">{t('不选择模型即为全部（all），模型列表自动跟随全局模型展示配置；选择后仅允许勾选项，别名会出现在该密钥的 /v1/models 中。')}</p><div className="mb-3 flex flex-wrap gap-2"><Input type="search" aria-label={t('搜索模型')} className="min-w-0 flex-1" value={search} onChange={event => setSearch(event.target.value)} placeholder={t('搜索模型')}/><Button type="button" size="sm" variant="outline" onClick={selectFilteredModels}>{t('全选搜索结果')}</Button><Button type="button" size="sm" variant={form.modelRules.length ? 'outline' : 'default'} aria-pressed={!form.modelRules.length} onClick={() => setForm({ ...form, modelRules: [] })}>{t('全部模型（all）')}</Button></div>
            {modelsQuery.error && <p role="alert" className="text-sm text-destructive">{modelsQuery.error.message}</p>}
            <div className="max-h-80 space-y-1 overflow-y-auto pr-1">{filteredModels.map(model => { const rule = form.modelRules.find(item => item.modelId === model.id); return <div key={model.id} className="rounded-lg border p-3"><label className="flex min-w-0 items-center gap-3 text-sm"><input type="checkbox" name="model" value={model.id} checked={!!rule} onChange={event => toggleModel(model.id, event.target.checked)} className="size-4 accent-primary"/><span className="min-w-0 flex-1 break-all font-mono">{model.id}</span>{model.cost && <span className="text-xs text-muted-foreground">{model.cost}</span>}</label>{rule && <label className="mt-3 grid gap-1 pl-7 text-xs text-muted-foreground">{t('模型别名（可选）')}<Input name={`alias-${model.id}`} value={rule.alias} onChange={event => setAlias(model.id, event.target.value)} maxLength={128} placeholder={model.id} className="font-mono text-sm"/></label>}</div> })}{!filteredModels.length && <p className="py-6 text-center text-sm text-muted-foreground">{modelsQuery.isLoading ? t('加载中…') : t('暂无可用模型')}</p>}</div>
          </fieldset>
        </div><SheetFooter className="flex-row justify-end border-t px-6 py-4"><Button type="button" variant="outline" onClick={() => setPanel(panel === 'edit' ? 'detail' : null)}>{t('取消')}</Button><Button type="submit" disabled={saving || modelsQuery.isLoading}>{saving ? t('保存中…') : t(panel === 'edit' ? '保存修改' : '创建 API 密钥')}</Button></SheetFooter></form>}
      </SheetContent>
    </Sheet>
  </>
}
