import { useEffect, useState, type FormEvent } from 'react'
import { useQuery } from '@tanstack/react-query'
import { Link } from '@tanstack/react-router'
import { ArrowUpRight } from 'lucide-react'
import { Button, Field, PageTitle, Section, useAdmin } from '../App'
import { get, post } from '../api'
import { SelectField } from '../components/app-select'
import type { AdminConfig, Model } from '../types'
export { ApiKeysSettingsPage } from './ApiKeys'

type ModelsData = { models: Model[]; lastSync?: { syncedAt?: string; changed?: boolean; added?: string[]; removed?: string[] } }
type OcConfig = { enabled: boolean; key: string; baseURL: string; maxConcurrency: number; retries: number; failover: boolean; failoverCount: number; failoverMinutes: number; proxyStrategy: string; proxies: string[]; proxyCooldowns?: Record<string, string>; syncedModels?: number; runtime?: { failoverActive?: boolean }; compaction?: { auto: boolean; buffer: number; keepTokens: number; maxSummary: number } }
type ClineProxy = { proxyStrategy: string; configuredProxies: string[] }
type UserRecord = { id: string; email: string; createdAt: string; disabledAt?: string }
const splitLines = (value: string) => value.split('\n').map(s => s.trim()).filter(Boolean)
const strategyOptions = [{ value: 'round_robin', label: 'Round Robin' }, { value: 'random', label: 'Random' }, { value: 'fill', label: 'Fill' }]

const modules = [
  { to: '/settings/general', title: '常规设置', description: '负载均衡、默认模型与监听地址' },
  { to: '/settings/api-keys', title: 'API 密钥', description: '创建、编辑与吊销，限制额度和模型' },
  { to: '/settings/security', title: '管理员与安全', description: '管理员账号与登录密码' },
  { to: '/settings/models', title: '模型配置', description: '模型同步、添加与上下文' },
  { to: '/settings/upstreams', title: '上游配置', description: 'OpenCode 和 Cline 出口代理' },
  { to: '/settings/advanced', title: '高级设置', description: '请求头与危险操作' },
] as const

export function SettingsHome() {
  const { t } = useAdmin()
  return <><PageTitle title={t('设置')} subtitle={t('按模块管理中转站配置')}/><div className="grid gap-4 md:grid-cols-2 xl:grid-cols-3">{modules.map(item => <Link key={item.to} to={item.to} className="group rounded-xl border bg-card p-5 shadow-sm transition-colors hover:border-primary/40 hover:bg-muted/30 focus-visible:outline-2 focus-visible:outline-primary"><div className="flex items-center justify-between"><h2 className="font-semibold">{t(item.title)}</h2><ArrowUpRight className="size-4 text-muted-foreground transition-transform group-hover:-translate-y-0.5 group-hover:translate-x-0.5"/></div><p className="mt-2 text-sm text-muted-foreground">{t(item.description)}</p></Link>)}</div></>
}
export function GeneralSettingsPage() {
  const { t } = useAdmin()
  const config = useQuery({ queryKey: ['config'], queryFn: async () => (await get<AdminConfig>('config')).data })
  const models = useQuery({ queryKey: ['models'], queryFn: async () => (await get<ModelsData>('models')).data })
  return <><PageTitle title={t('常规设置')} subtitle={t('负载均衡、模型默认值与监听地址')}/>{config.data && <General initial={config.data} models={models.data?.models || []}/>}</>
}
export function SecuritySettingsPage() {
  const { t } = useAdmin()
  const users = useQuery({ queryKey: ['admin-users'], queryFn: async () => (await get<{ users: UserRecord[] }>('users')).data.users || [] })
  return <><PageTitle title={t('管理员与安全')} subtitle={t('管理后台登录身份与密码')}/><PasswordSettings/><Users users={users.data || []}/></>
}
export function ModelsSettingsPage() {
  const { t } = useAdmin()
  const models = useQuery({ queryKey: ['models'], queryFn: async () => (await get<ModelsData>('models')).data })
  return <><PageTitle title={t('模型配置')} subtitle={t('模型同步、添加与上下文')}/><Models data={models.data}/></>
}
export function UpstreamsSettingsPage() {
  const { t } = useAdmin()
  const oc = useQuery({ queryKey: ['opencode-config'], queryFn: async () => (await get<OcConfig>('opencode/config')).data })
  const cline = useQuery({ queryKey: ['cline-proxy-config'], queryFn: async () => (await get<ClineProxy>('cline-proxy/config')).data })
  return <><PageTitle title={t('上游配置')} subtitle={t('OpenCode 与 Cline 出口代理')}/>{oc.data && <OpenCode initial={oc.data}/>} {cline.data && <ClineProxySettings initial={cline.data}/>}</>
}
export function AdvancedSettingsPage() {
  const { t } = useAdmin()
  const config = useQuery({ queryKey: ['config'], queryFn: async () => (await get<AdminConfig>('config')).data })
  return <><PageTitle title={t('高级设置')} subtitle={t('请求头和危险操作')}/>{config.data && <Headers initial={config.data.headers || {}}/>}<Section title={t('危险操作')}><Danger path="accounts/delete-all" label={t('删除全部账号')}/></Section></>
}
function General({ initial, models }: { initial: AdminConfig; models: Model[] }) {
  const { t, run, notify } = useAdmin()
  const [strategy, setStrategy] = useState(initial.strategy)
  const [defaultModel, setDefaultModel] = useState(initial.defaultModel)
  const [effort, setEffort] = useState(initial.anthropicEffort)
  const [host, setHost] = useState(initial.host)
  useEffect(() => { setStrategy(initial.strategy); setDefaultModel(initial.defaultModel); setEffort(initial.anthropicEffort); setHost(initial.host) }, [initial])
  return <><Section title={t('代理设置')}><div className="grid gap-4 sm:grid-cols-2 lg:grid-cols-3"><SelectField label={t('负载均衡策略')} value={strategy} onValueChange={setStrategy} options={strategyOptions}/><SelectField label={t('默认模型')} value={defaultModel} onValueChange={setDefaultModel} options={[{ value: '', label: t('自动选择') }, ...models.map(m => ({ value: m.id, label: m.id }))]}/><SelectField label={t('Anthropic 推理强度')} value={effort} onValueChange={setEffort} options={[{ value: 'low', label: 'low' }, { value: 'medium', label: 'medium' }, { value: 'high', label: 'high' }]}/></div><div className="mt-4"><Button onClick={() => run(() => post('config/update', { strategy, defaultModel, anthropicEffort: effort }), '配置已更新')}>{t('保存配置')}</Button></div></Section>
    <Section title={t('监听地址')}><p className="mb-3 text-sm text-neutral-500">{initial.address} · {initial.poolPath}</p><div className="flex flex-wrap items-end gap-3"><SelectField label={t('监听网卡')} value={host} onValueChange={setHost} options={[{ value: '127.0.0.1', label: '127.0.0.1' }, { value: '0.0.0.0', label: '0.0.0.0' }, ...(initial.localIPs || []).map(ip => ({ value: ip, label: ip }))]}/><Button onClick={() => run(async () => { const response = await post<{ address: string }>('config/update', { host }); notify(`${t('监听已切换')}: ${response.data.address}`) })}>{t('保存')}</Button></div></Section>
  </>
}
function PasswordSettings() {
  const { t, run } = useAdmin()
  const [currentPassword, setCurrentPassword] = useState('')
  const [newPassword, setNewPassword] = useState('')
  return <Section title={t('修改登录密码')}><form className="flex flex-wrap items-end gap-3" onSubmit={async event => { event.preventDefault(); if (await run(() => post('password', { currentPassword, newPassword }), '密码已更新')) { setCurrentPassword(''); setNewPassword('') } }}><Field label={t('当前密码')}><input type="password" autoComplete="current-password" required value={currentPassword} onChange={event => setCurrentPassword(event.target.value)}/></Field><Field label={t('新密码')}><input type="password" autoComplete="new-password" minLength={12} required value={newPassword} onChange={event => setNewPassword(event.target.value)}/></Field><Button type="submit">{t('更新密码')}</Button></form></Section>
}
function Users({ users }: { users: UserRecord[] }) {
  const { t, run, user } = useAdmin()
  const [email, setEmail] = useState('')
  const [password, setPassword] = useState('')
  return <Section title={t('管理员账号')}><form className="mb-4 flex flex-wrap items-end gap-3" onSubmit={async event => { event.preventDefault(); if (await run(() => post('users', { email, password }), '管理员已创建')) { setEmail(''); setPassword('') } }}><Field label={t('邮箱')}><input type="email" autoComplete="off" required value={email} onChange={event => setEmail(event.target.value)}/></Field><Field label={t('初始密码')}><input type="password" autoComplete="new-password" minLength={12} required value={password} onChange={event => setPassword(event.target.value)}/></Field><Button type="submit">{t('添加管理员')}</Button></form><div className="divide-y rounded-lg border">{users.map(item => <div key={item.id} className="flex flex-wrap items-center gap-3 p-3 text-sm"><span className="min-w-0 flex-1 truncate font-medium">{item.email}{item.id === user?.id && ` (${t('当前账号')})`}</span><span className="text-muted-foreground">{item.disabledAt ? t('已停用') : t('活跃')}</span><button type="button" className="text-primary hover:underline disabled:cursor-not-allowed disabled:opacity-50" disabled={item.id === user?.id} onClick={() => confirm(`${t(item.disabledAt ? '启用管理员？' : '停用管理员？')} ${item.email}`) && run(() => post('users/status', { id: item.id, disabled: !item.disabledAt }), item.disabledAt ? '管理员已启用' : '管理员已停用')}>{t(item.disabledAt ? '启用' : '停用')}</button></div>)}</div></Section>
}
function Models({ data }: { data?: ModelsData }) {
  const { t, run } = useAdmin()
  const [id, setId] = useState('')
  const [cost, setCost] = useState('pass')
  const [selected, setSelected] = useState('')
  const [context, setContext] = useState(0)
  const [output, setOutput] = useState(0)
  const [syncResult, setSyncResult] = useState('')
  const models = data?.models || []
  const groups = [
    ['opencode · 免费模型', models.filter(m => (m.source === 'zen' || m.provider === 'opencode') && m.cost === 'free')],
    ['opencode · 付费模型', models.filter(m => (m.source === 'zen' || m.provider === 'opencode') && m.cost !== 'free')],
    ['第三方渠道模型', models.filter(m => m.source === 'custom_provider')],
    ['Cline · 免费模型', models.filter(m => m.source !== 'custom_provider' && m.source !== 'zen' && m.provider !== 'opencode' && !m.custom && m.cost === 'free')],
    ['Cline · 付费模型', models.filter(m => m.source !== 'custom_provider' && m.source !== 'zen' && m.provider !== 'opencode' && !m.custom && m.cost !== 'free')],
    ['用户自定义', models.filter(m => m.custom)],
  ] as const
  async function sync(path: string) { await run(async () => { const result = await post<{ added?: string[]; removed?: string[] }>(path); setSyncResult(`${t('新增模型')}: ${result.data?.added?.join(', ') || '-'}; ${t('已下架')}: ${result.data?.removed?.join(', ') || '-'}`) }, '模型已同步') }
  return <><Section title={t('模型列表')} aside={<span className="text-xs text-neutral-500">{models.length} · {data?.lastSync?.syncedAt || t('从未同步')}</span>}><div className="mb-4 flex flex-wrap gap-2"><Button onClick={() => sync('models/sync')}>{t('同步 Cline 模型')}</Button><Button onClick={() => sync('opencode/models/sync')}>{t('同步 opencode 模型')}</Button></div>{syncResult && <p className="mb-3 text-sm">{syncResult}</p>}{groups.map(([label, items]) => items.length > 0 && <details key={label} open={!label.includes('付费')} className="mb-3 rounded-lg border border-neutral-200 p-3"><summary className="cursor-pointer font-medium">{t(label)} · {items.length}</summary><div className="mt-3 flex flex-wrap gap-2">{items.map(m => <span key={m.id} className="inline-flex items-center gap-1 rounded-full bg-neutral-100 px-3 py-1 text-xs"><span>{m.id}{m.delisted && ` (${t('已下架')})`}</span>{(m.custom || m.delisted) && <button aria-label={`${t('删除')} ${m.id}`} className="text-red-600" onClick={() => confirm(`${t('确认删除模型')} ${m.id}?`) && run(() => post('models/delete', { id: m.id }), '模型已删除')}>✕</button>}</span>)}</div></details>)}</Section>
    <Section title={t('添加自定义模型')}><form className="flex flex-wrap items-end gap-3" onSubmit={(e: FormEvent) => { e.preventDefault(); run(() => post('models/add', { id, cost }), '模型已添加').then(() => setId('')) }}><Field label={t('模型 ID')}><input required value={id} onChange={e => setId(e.target.value)}/></Field><SelectField label={t('类型')} value={cost} onValueChange={setCost} options={[{ value: 'free', label: 'free' }, { value: 'pass', label: 'pass' }]}/><Button type="submit">{t('添加')}</Button></form></Section>
    <Section title={t('模型上下文设置')}><form className="flex flex-wrap items-end gap-3" onSubmit={(e: FormEvent) => { e.preventDefault(); if (selected) run(() => post('models/context', { id: selected, context, output }), '已保存') }}><SelectField label={t('模型')} value={selected} onValueChange={value => { setSelected(value); const m = models.find(x => x.id === value); setContext(m?.context || 0); setOutput(m?.output || 0) }} options={[{ value: '', label: t('选择模型') }, ...models.map(m => ({ value: m.id, label: m.id }))]}/><Field label="Context"><input type="number" min={0} value={context} onChange={e => setContext(Number(e.target.value))}/></Field><Field label="Output"><input type="number" min={0} value={output} onChange={e => setOutput(Number(e.target.value))}/></Field><Button type="submit" disabled={!selected}>{t('保存')}</Button><button type="button" className="btn" disabled={!selected} onClick={() => { setContext(0); setOutput(0); run(() => post('models/context', { id: selected, context: 0, output: 0 }), '已清除') }}>{t('清除')}</button></form></Section>
  </>
}
function OpenCode({ initial }: { initial: OcConfig }) {
  const { t, run } = useAdmin()
  const [form, setForm] = useState(initial)
  const [proxies, setProxies] = useState('')
  useEffect(() => setForm(initial), [initial])
  const update = (patch: Partial<OcConfig>) => setForm({ ...form, ...patch })
  return <Section title={t('opencode 配置')}><form onSubmit={(e: FormEvent) => { e.preventDefault(); const payload = { enabled: form.enabled, key: form.key, baseURL: form.baseURL, maxConcurrency: form.maxConcurrency, retries: form.retries, failover: form.failover, failoverCount: form.failoverCount, failoverMinutes: form.failoverMinutes, proxyStrategy: form.proxyStrategy, compaction: form.compaction, ...(proxies.trim() ? { proxies: splitLines(proxies) } : {}) }; run(() => post('opencode/config/update', payload), 'opencode 配置已保存') }} className="grid gap-4 sm:grid-cols-2 lg:grid-cols-3"><SelectField label={t('启用')} value={String(form.enabled)} onValueChange={value => update({ enabled: value === 'true' })} options={[{ value: 'true', label: t('是') }, { value: 'false', label: t('否') }]}/><Field label="API Key"><input type="password" value={form.key || ''} onChange={e => update({ key: e.target.value })}/></Field><Field label="Base URL"><input value={form.baseURL || ''} onChange={e => update({ baseURL: e.target.value })}/></Field><Field label={t('最大并发')}><input type="number" min={1} value={form.maxConcurrency} onChange={e => update({ maxConcurrency: Number(e.target.value) })}/></Field><Field label={t('重试次数')}><input type="number" min={0} value={form.retries} onChange={e => update({ retries: Number(e.target.value) })}/></Field><SelectField label={t('故障转移')} value={String(form.failover)} onValueChange={value => update({ failover: value === 'true' })} options={[{ value: 'true', label: t('是') }, { value: 'false', label: t('否') }]}/><Field label={t('故障阈值')}><input type="number" min={1} value={form.failoverCount} onChange={e => update({ failoverCount: Number(e.target.value) })}/></Field><Field label={t('故障窗口（分钟）')}><input type="number" min={1} value={form.failoverMinutes} onChange={e => update({ failoverMinutes: Number(e.target.value) })}/></Field><SelectField label={t('出口策略')} value={form.proxyStrategy || 'round_robin'} onValueChange={proxyStrategy => update({ proxyStrategy })} options={strategyOptions}/><SelectField label={t('自动压缩')} value={String(form.compaction?.auto || false)} onValueChange={value => update({ compaction: { auto: value === 'true', buffer: form.compaction?.buffer || 20000, keepTokens: form.compaction?.keepTokens || 8000, maxSummary: form.compaction?.maxSummary || 4096 } })} options={[{ value: 'true', label: t('是') }, { value: 'false', label: t('否') }]}/>{([['buffer', 'Buffer'], ['keepTokens', 'Keep Tokens'], ['maxSummary', 'Max Summary']] as const).map(([key, label]) => <Field key={key} label={label}><input type="number" min={0} value={form.compaction?.[key] || 0} onChange={e => update({ compaction: { auto: form.compaction?.auto || false, buffer: form.compaction?.buffer || 0, keepTokens: form.compaction?.keepTokens || 0, maxSummary: form.compaction?.maxSummary || 0, [key]: Number(e.target.value) } })}/></Field>)}<div className="sm:col-span-2 lg:col-span-3"><Field label={t('代理列表（留空保持现有配置）')}><textarea rows={3} value={proxies} onChange={e => setProxies(e.target.value)} placeholder={(initial.proxies || []).join('\n')}/></Field></div><div><Button type="submit">{t('保存')}</Button></div><div className="self-center text-sm text-neutral-500">{form.runtime?.failoverActive ? t('故障转移中') : t('正常')} · {t('已同步模型')}: {form.syncedModels || 0}</div></form></Section>
}
function ClineProxySettings({ initial }: { initial: ClineProxy }) {
  const { t, run } = useAdmin()
  const [strategy, setStrategy] = useState(initial.proxyStrategy)
  const [proxies, setProxies] = useState('')
  const [clear, setClear] = useState(false)
  useEffect(() => setStrategy(initial.proxyStrategy), [initial])
  return <Section title={t('Cline 出口代理')}><p className="mb-3 text-sm text-neutral-500">{t('已配置')}: {(initial.configuredProxies || []).join(' · ') || t('未配置')}</p><form className="grid gap-4 sm:grid-cols-2" onSubmit={(e: FormEvent) => { e.preventDefault(); const payload = { proxyStrategy: strategy, ...(clear ? { clearProxies: true } : proxies.trim() ? { proxies: splitLines(proxies) } : {}) }; run(() => post('cline-proxy/config/update', payload), 'Cline 出口代理配置已保存').then(() => { setProxies(''); setClear(false) }) }}><SelectField label={t('出口策略')} value={strategy} onValueChange={setStrategy} options={strategyOptions}/><Field label={t('新代理列表（留空保持不变）')}><textarea rows={3} value={proxies} onChange={e => setProxies(e.target.value)}/></Field><label className="flex items-center gap-2 text-sm"><input type="checkbox" checked={clear} onChange={e => setClear(e.target.checked)}/>{t('清除代理列表')}</label><div><Button type="submit">{t('保存')}</Button></div></form></Section>
}
function Headers({ initial }: { initial: Record<string, string> }) {
  const { t, run } = useAdmin()
  const [rows, setRows] = useState(Object.entries(initial).map(([key, value]) => ({ key, value })))
  useEffect(() => setRows(Object.entries(initial).map(([key, value]) => ({ key, value }))), [initial])
  return <Section title={t('请求头配置')}><div className="grid gap-2">{rows.map((row, i) => <div key={i} className="grid grid-cols-[1fr_1fr_auto] gap-2"><input aria-label={t('请求头')} placeholder="Header-Name" value={row.key} onChange={e => setRows(rows.map((r, j) => j === i ? { ...r, key: e.target.value } : r))}/><input aria-label={t('值')} placeholder="value" value={row.value} onChange={e => setRows(rows.map((r, j) => j === i ? { ...r, value: e.target.value } : r))}/><button className="btn btn-sm btn-danger" onClick={() => setRows(rows.filter((_, j) => j !== i))} aria-label={t('删除')}>✕</button></div>)}</div><div className="mt-3 flex gap-2"><button className="btn" onClick={() => setRows([...rows, { key: '', value: '' }])}>{t('添加请求头')}</button><Button onClick={() => run(() => post('config/update', { headers: Object.fromEntries(rows.filter(r => r.key.trim()).map(r => [r.key.trim(), r.value.trim()])) }), '请求头已保存')}>{t('保存请求头')}</Button></div></Section>
}
function Danger({ path, label }: { path: string; label: string }) {
  const { t, run } = useAdmin()
  return <Button danger onClick={() => { if (!confirm(`${t('确定')} ${label}?`)) return; run(() => post(path), label) }}>{label}</Button>
}
export function About() {
  const { t } = useAdmin()
  const config = useQuery({ queryKey: ['config'], queryFn: async () => (await get<AdminConfig>('config')).data })
  return <><PageTitle title={t('关于')} subtitle={t('应用信息、使用指南与开源协议')}/><Section title={t('应用信息')}><div className="text-xl font-semibold">Cline2API</div><p className="text-sm text-neutral-500">Cline API 反向代理 · 多账号轮询 · 双协议兼容</p><p className="mt-2 text-sm">{t('版本')} {config.data?.version || 'dev'} · MIT License · Go + React</p></Section><Section title={t('快速上手')}><ol className="list-inside list-decimal space-y-3 text-sm"><li>{t('前往“导入账号”通过 OAuth 或 Refresh Token 添加账号')}</li><li>{t('前往“设置”生成 API Key')}</li><li>{t('配置客户端指向本代理地址')}</li></ol></Section></>
}
