import { useEffect, useState, type FormEvent } from 'react'
import { useQuery } from '@tanstack/react-query'
import { Button, Field, PageTitle, Section, duration, useAdmin } from '../App'
import { get, post } from '../api'
import { SelectField } from '../components/app-select'
import type { Model, Provider, ProviderModel } from '../types'

type VisibilityData = { models: Model[]; configured: boolean; modelIds: string[] }
export function Visibility() {
  const { t, run } = useAdmin()
  const query = useQuery({ queryKey: ['model-visibility'], queryFn: async () => (await get<VisibilityData>('models/visibility')).data })
  const [selected, setSelected] = useState<string[] | null>(null)
  const [filter, setFilter] = useState('')
  const models = query.data?.models || []
  const chosen = selected || (query.data?.configured ? query.data.modelIds || [] : models.map(m => m.id))
  function setAll(on: boolean) { setSelected(on ? models.map(m => m.id) : []) }
  return <><PageTitle title={t('模型展示')} subtitle={t('配置 /v1/models 展示列表')}/><Section title={t('可用模型')} aside={<span className="text-xs text-neutral-500">{chosen.length} / {models.length}</span>}>
    <div className="mb-4 flex flex-wrap gap-2"><input aria-label={t('搜索模型')} placeholder={t('搜索模型')} value={filter} onChange={e => setFilter(e.target.value)}/><button className="btn btn-sm" onClick={() => setAll(true)}>{t('全选')}</button><button className="btn btn-sm" onClick={() => setAll(false)}>{t('清空')}</button></div>
    {query.error && <p role="alert">{query.error.message}</p>}
    <div className="grid max-h-[60vh] gap-2 overflow-y-auto sm:grid-cols-2 lg:grid-cols-3">{models.filter(m => m.id.toLowerCase().includes(filter.toLowerCase())).map(m => <label key={m.id} className="flex min-w-0 items-center gap-2 rounded-lg border border-neutral-200 p-2 text-sm"><input type="checkbox" checked={chosen.includes(m.id)} onChange={e => setSelected(e.target.checked ? [...chosen, m.id] : chosen.filter(x => x !== m.id))}/><span className="truncate" title={m.id}>{m.id}</span><span className="ml-auto text-xs text-neutral-400">{m.cost}</span></label>)}</div>
    <div className="mt-5 flex gap-2"><Button onClick={() => run(() => post('models/visibility', { configured: true, modelIds: models.filter(m => chosen.includes(m.id)).map(m => m.id) }), '模型展示列表已保存').then(() => setSelected(null))}>{t('保存')}</Button><button className="btn" onClick={() => run(() => post('models/visibility', { configured: false, modelIds: [] }), '已恢复默认').then(() => setSelected(null))}>{t('恢复默认')}</button></div>
  </Section></>
}

type ProvidersData = { strategy: string; providers: Provider[] }
const emptyProvider = { id: '', name: '', protocol: 'openai', forceStream: false, allowPrivateNetwork: false, baseURL: '', apiKey: '', enabled: true }
export function Providers() {
  const { t, run } = useAdmin()
  const query = useQuery({ queryKey: ['providers'], queryFn: async () => (await get<ProvidersData>('providers')).data })
  const [form, setForm] = useState(emptyProvider)
  const [strategy, setStrategy] = useState<string | null>(null)
  const providers = query.data?.providers || []
  function edit(p: Provider) { setForm({ id: p.id, name: p.name, protocol: p.protocol, forceStream: p.forceStream, allowPrivateNetwork: p.allowPrivateNetwork, baseURL: p.baseURL, apiKey: '', enabled: p.enabled }); scrollTo({ top: 0, behavior: 'smooth' }) }
  async function save(event: FormEvent) { event.preventDefault(); await run(() => post('providers/save', form), '渠道已保存'); setForm(emptyProvider) }
  return <><PageTitle title={t('第三方渠道')} subtitle={t('管理自定义上游和模型映射')}/>
    <Section title={form.id ? t('编辑渠道') : t('添加渠道')}><form onSubmit={save} className="grid gap-4 sm:grid-cols-2"><Field label={t('名称')}><input required value={form.name} onChange={e => setForm({ ...form, name: e.target.value })}/></Field><SelectField label={t('协议')} value={form.protocol} onValueChange={protocol => setForm({ ...form, protocol })} options={[{ value: 'openai', label: 'OpenAI Chat Completions' }, { value: 'responses', label: 'OpenAI Responses' }, { value: 'anthropic', label: 'Anthropic Messages' }]}/><Field label={t('上游地址')}><input required type="url" value={form.baseURL} onChange={e => setForm({ ...form, baseURL: e.target.value })} placeholder="https://example.com/v1"/></Field><Field label="API Key"><input type="password" value={form.apiKey} onChange={e => setForm({ ...form, apiKey: e.target.value })} placeholder={form.id ? t('编辑时留空表示保持原 Key') : ''}/></Field><SelectField label={t('状态')} value={String(form.enabled)} onValueChange={value => setForm({ ...form, enabled: value === 'true' })} options={[{ value: 'true', label: t('已启用') }, { value: 'false', label: t('已停用') }]}/><SelectField label={t('允许私有网络')} value={String(form.allowPrivateNetwork)} onValueChange={value => setForm({ ...form, allowPrivateNetwork: value === 'true' })} options={[{ value: 'false', label: t('否') }, { value: 'true', label: t('是') }]}/>{form.protocol === 'responses' && <SelectField label={t('强制流式')} value={String(form.forceStream)} onValueChange={value => setForm({ ...form, forceStream: value === 'true' })} options={[{ value: 'false', label: t('否') }, { value: 'true', label: t('是') }]}/>}<div className="flex items-end gap-2"><Button type="submit">{t('保存渠道')}</Button>{form.id && <button type="button" className="btn" onClick={() => setForm(emptyProvider)}>{t('取消')}</button>}</div></form></Section>
    <Section title={t('渠道负载均衡')}><div className="flex items-end gap-3"><SelectField label={t('策略')} value={strategy ?? query.data?.strategy ?? 'round_robin'} onValueChange={setStrategy} options={[{ value: 'round_robin', label: 'Round Robin' }, { value: 'random', label: 'Random' }, { value: 'fill', label: 'Fill' }]}/><Button onClick={() => run(() => post('providers/strategy', { strategy: strategy ?? query.data?.strategy ?? 'round_robin' }), '策略已保存')}>{t('保存')}</Button></div></Section>
    <Section title={t('已配置渠道')}>{query.error && <p role="alert">{query.error.message}</p>}{!providers.length && <p className="empty">{t('尚未配置第三方渠道')}</p>}<div className="grid gap-4">{providers.map(p => <article key={p.id} className="rounded-xl border border-neutral-200 p-4"><div className="flex flex-wrap justify-between gap-3"><div><h3 className="font-semibold">{p.name} <small className="text-neutral-500">{p.enabled ? t('启用') : t('停用')}</small></h3><p className="break-all text-sm text-neutral-500">{p.protocol} · {p.baseURL}</p><p className="text-xs text-neutral-500">API Key: {p.keyPreview || '-'} · {t('上次成功')}: {p.runtime?.lastSuccess || '-'}</p></div><div className="flex gap-2"><button className="btn btn-sm" onClick={() => edit(p)}>{t('编辑')}</button><button className="btn btn-sm" onClick={() => run(() => post('providers/models/sync', { id: p.id }), '渠道模型已同步')}>{t('同步模型')}</button><button className="btn btn-sm btn-danger" onClick={() => confirm(t('确定删除此渠道？')) && run(() => post('providers/delete', { id: p.id }), '渠道已删除')}>{t('删除')}</button></div></div>{p.runtime?.lastError && <p className="mt-2 rounded bg-red-50 p-2 text-sm text-red-700">{p.runtime.lastError}</p>}<ProviderModels key={`${p.id}-${p.models.length}`} provider={p}/></article>)}</div></Section>
  </>
}
function ProviderModels({ provider }: { provider: Provider }) {
  const { t, run } = useAdmin()
  const [models, setModels] = useState<ProviderModel[]>(provider.models || [])
  const [upstream, setUpstream] = useState('')
  const [publicId, setPublicId] = useState('')
  useEffect(() => setModels(provider.models || []), [provider.models])
  function update(index: number, patch: Partial<ProviderModel>) { setModels(models.map((m, i) => i === index ? { ...m, ...patch } : m)) }
  async function save(next = models) { if (next.some(m => !m.publicId.trim())) return; await run(() => post('providers/models/update', { id: provider.id, models: next }), '模型映射已保存') }
  return <details className="mt-3 border-t border-neutral-200 pt-3"><summary className="cursor-pointer text-sm font-semibold">{t('模型映射')} · {models.length}</summary><div className="mt-3 grid gap-2">{models.map((m, i) => <div key={`${m.id}-${i}`} className="grid grid-cols-[auto_1fr_1fr] items-center gap-2 text-sm"><input type="checkbox" checked={m.enabled} onChange={e => update(i, { enabled: e.target.checked })} aria-label={`${t('启用')} ${m.id}`}/><span className="truncate font-mono" title={m.id}>{m.id} {provider.runtime?.latencies?.[m.id] && `· ${duration(provider.runtime.latencies[m.id].ewmaMs)}`}</span><input aria-label={t('公开 Model ID')} value={m.publicId || m.id} onChange={e => update(i, { publicId: e.target.value })}/></div>)}<div className="mt-2 flex flex-wrap gap-2"><input aria-label={t('上游 Model ID')} placeholder={t('上游 Model ID')} value={upstream} onChange={e => setUpstream(e.target.value)}/><input aria-label={t('公开 Model ID')} placeholder={t('公开 Model ID')} value={publicId} onChange={e => setPublicId(e.target.value)}/><button className="btn btn-sm" onClick={() => { if (!upstream.trim()) return; const next = [...models, { id: upstream.trim(), publicId: publicId.trim() || upstream.trim(), enabled: true }]; setModels(next); save(next); setUpstream(''); setPublicId('') }}>{t('添加')}</button><Button onClick={() => save()}>{t('保存模型映射')}</Button></div></div></details>
}
