import { useState } from 'react'
import { useNavigate, useSearch } from '@tanstack/react-router'
import { useQuery } from '@tanstack/react-query'
import { Activity, ArrowUpRight, CheckCircle2, Clock3, Gauge, KeyRound, Layers3 } from 'lucide-react'
import { Link } from '@tanstack/react-router'
import { Button, PageTitle, Section, duration, number, tokens, useAdmin } from '../App'
import { get } from '../api'
import { AppSelect } from '../components/app-select'

type Usage = { requests: number; inputTokens: number; outputTokens: number; cachedTokens: number; totalTokens: number }
type Breakdown = Usage & { name: string; completed: number; avgOutputTps: number }
type Performance = { completed: number; failed: number; avgOutputTps: number; speedSamples: number; avgTtftMs: number; ttftSamples: number }
type Period = { summary: Usage; days: (Usage & { date: string })[]; models: Breakdown[]; upstreams: Breakdown[]; performance: Performance }
type Stats = { total: number; active: number; cooldown: number; expired: number; todayUsage: Period; periodUsage: Period }
const ranges = ['today', '1d', '7d', '14d', '30d', 'all'] as const

function Metric({ label, value, detail, icon: Icon }: { label: string; value: string; detail: string; icon: typeof Activity }) {
  return <div className="rounded-xl border bg-card p-5 shadow-sm"><div className="flex items-center justify-between text-sm text-muted-foreground"><span>{label}</span><Icon className="size-4" aria-hidden="true"/></div><div className="mt-4 text-3xl font-semibold tracking-tight tabular-nums">{value}</div><p className="mt-2 text-xs text-muted-foreground">{detail}</p></div>
}

export function Dashboard() {
  const { t } = useAdmin()
  const { range: selectedRange } = useSearch({ from: '/' })
  const range = selectedRange || 'today'
  const navigate = useNavigate({ from: '/' })
  const [refreshMs, setRefreshMs] = useState(Number(localStorage.getItem('cline_stats_refresh_interval')) || 10000)
  const stats = useQuery({ queryKey: ['stats', range], queryFn: async () => (await get<Stats>(`stats?range=${range}&tzOffset=${new Date().getTimezoneOffset()}`)).data, refetchInterval: refreshMs })
  const today = stats.data?.todayUsage
  const selected = stats.data?.periodUsage
  const days = [...(selected?.days || [])].reverse()
  const maxRequests = Math.max(1, ...days.map(day => day.requests))
  const completionRate = today?.summary.requests ? Math.round(100 * today.performance.completed / today.summary.requests) : 0
  return <>
    <PageTitle title={t('概览')} subtitle={t('中转站实时概况与模型调用')} action={<Button onClick={() => stats.refetch()}>{t('刷新')}</Button>}/>
    <div className="mb-5 flex flex-wrap items-center gap-2"><span className="mr-1 text-sm text-muted-foreground">{t('统计范围')}</span>{ranges.map(key => <button key={key} type="button" aria-pressed={range === key} className={`rounded-full px-3 py-1.5 text-sm font-medium transition-colors ${range === key ? 'bg-primary text-primary-foreground' : 'bg-card text-muted-foreground hover:bg-muted'}`} onClick={() => navigate({ to: '/', search: { range: key } })}>{key === 'today' ? t('今天') : key === 'all' ? t('全部') : key}</button>)}<div className="ml-auto flex items-center gap-2 text-sm text-muted-foreground"><span>{t('自动刷新')}</span><AppSelect ariaLabel={t('自动刷新')} className="w-24" value={String(refreshMs)} onValueChange={value => { const interval = Number(value); setRefreshMs(interval); localStorage.setItem('cline_stats_refresh_interval', String(interval)) }} options={[{ value: '5000', label: '5s' }, { value: '10000', label: '10s' }, { value: '30000', label: '30s' }]}/></div></div>
    {stats.error && <p role="alert" className="mb-4 rounded-md border border-destructive/30 bg-destructive/10 p-3 text-sm text-destructive">{t('加载失败')}: {stats.error.message}</p>}
    <div className="mb-5 grid gap-3 sm:grid-cols-2 xl:grid-cols-4">
      <Metric label={t('今日调用')} value={number(today?.summary.requests)} detail={t('今天进入中转站的请求')} icon={Activity}/>
      <Metric label={t('今日 Token')} value={tokens(today?.summary.totalTokens)} detail={`${t('输入')} ${tokens(today?.summary.inputTokens)} · ${t('输出')} ${tokens(today?.summary.outputTokens)} · ${t('缓存')} ${tokens(today?.summary.cachedTokens)}`} icon={Layers3}/>
      <Metric label={t('今日输出速度')} value={today?.performance.speedSamples ? `${today.performance.avgOutputTps.toFixed(1)} tok/s` : '-'} detail={`${t('成功请求平均值')} · ${number(today?.performance.speedSamples)} ${t('个样本')}`} icon={Gauge}/>
      <Metric label={t('今日成功率')} value={today?.summary.requests ? `${completionRate}%` : '-'} detail={`${number(today?.performance.completed)} ${t('成功')} · ${number(today?.performance.failed)} ${t('失败')}`} icon={CheckCircle2}/>
    </div>
    <div className="grid gap-5 xl:grid-cols-[minmax(0,2fr)_minmax(300px,1fr)]">
      <Section title={t('请求趋势')} aside={<span className="text-xs text-muted-foreground">{t('按访问者时区统计')}</span>}><div className="flex h-44 items-end gap-1.5 overflow-x-auto border-b pb-2" role="img" aria-label={t('每日请求量柱状图')}>{days.map(day => <div key={day.date} title={`${day.date}: ${number(day.requests)} ${t('请求')}`} className="group flex h-full min-w-5 flex-1 flex-col items-center justify-end gap-1"><span className="text-[10px] tabular-nums text-muted-foreground opacity-0 group-hover:opacity-100">{day.requests}</span><div className="w-full min-w-2 rounded-t bg-primary/75 transition-colors group-hover:bg-primary" style={{ height: `${Math.max(day.requests ? 8 : 2, 100 * day.requests / maxRequests)}%` }}/></div>)}</div><div className="mt-2 flex justify-between text-xs text-muted-foreground"><span>{days[0]?.date || '-'}</span><span>{days.at(-1)?.date || '-'}</span></div><div className="mt-4 grid gap-3 border-t pt-4 text-sm sm:grid-cols-3"><div><span className="text-muted-foreground">{t('范围调用')}</span><strong className="ml-2 tabular-nums">{number(selected?.summary.requests)}</strong></div><div><span className="text-muted-foreground">{t('范围 Token')}</span><strong className="ml-2 tabular-nums">{tokens(selected?.summary.totalTokens)}</strong></div><div><span className="text-muted-foreground">{t('平均首字延迟')}</span><strong className="ml-2 tabular-nums">{selected?.performance.ttftSamples ? duration(selected.performance.avgTtftMs) : '-'}</strong></div></div></Section>
      <Section title={t('账号健康')} aside={<Link to="/accounts" className="inline-flex items-center gap-1 text-xs text-primary hover:underline">{t('查看账号')}<ArrowUpRight className="size-3"/></Link>}><div className="mb-4 flex items-baseline gap-2"><span className="text-3xl font-semibold tabular-nums">{number(stats.data?.active)}</span><span className="text-sm text-muted-foreground">/ {number(stats.data?.total)} {t('活跃账号')}</span></div><div className="grid grid-cols-2 gap-3 text-sm"><div className="rounded-lg bg-muted/50 p-3"><div className="text-muted-foreground">{t('冷却')}</div><strong className="mt-1 block text-lg tabular-nums">{number(stats.data?.cooldown)}</strong></div><div className="rounded-lg bg-muted/50 p-3"><div className="text-muted-foreground">{t('已过期')}</div><strong className="mt-1 block text-lg tabular-nums">{number(stats.data?.expired)}</strong></div></div><div className="mt-4 flex items-center gap-2 border-t pt-4 text-sm"><Clock3 className="size-4 text-muted-foreground"/><span className="text-muted-foreground">{t('范围平均输出速度')}</span><strong className="ml-auto tabular-nums">{selected?.performance.speedSamples ? `${selected.performance.avgOutputTps.toFixed(1)} tok/s` : '-'}</strong></div></Section>
    </div>
    <Section title={t('模型调用')} aside={<span className="text-xs text-muted-foreground">{t('按调用次数排序')}</span>}><div className="overflow-x-auto"><table className="w-full min-w-[720px]"><thead><tr>{['模型', '请求', '总 Token', '输入', '输出', '成功率', '平均输出速度'].map(label => <th key={label}>{t(label)}</th>)}</tr></thead><tbody>{(selected?.models || []).map(model => <tr key={model.name}><td className="max-w-64 truncate font-mono text-xs" title={model.name}>{model.name}</td><td>{number(model.requests)}</td><td>{tokens(model.totalTokens)}</td><td>{tokens(model.inputTokens)}</td><td>{tokens(model.outputTokens)}</td><td>{model.requests ? `${Math.round(100 * model.completed / model.requests)}%` : '-'}</td><td>{model.avgOutputTps ? `${model.avgOutputTps.toFixed(1)} tok/s` : '-'}</td></tr>)}</tbody></table>{!selected?.models.length && <p className="py-8 text-center text-sm text-muted-foreground">{t('暂无请求日志')}</p>}</div></Section>
    <Section title={t('渠道分布')} aside={<Link to="/providers" className="inline-flex items-center gap-1 text-xs text-primary hover:underline">{t('管理渠道')}<ArrowUpRight className="size-3"/></Link>}><div className="grid gap-3 md:grid-cols-2 xl:grid-cols-3">{(selected?.upstreams || []).map(upstream => <div key={upstream.name} className="rounded-lg border p-4"><div className="flex items-center justify-between gap-3"><span className="truncate text-sm font-medium" title={upstream.name}>{upstream.name}</span><span className="text-xs tabular-nums text-muted-foreground">{selected?.summary.requests ? Math.round(100 * upstream.requests / selected.summary.requests) : 0}%</span></div><div className="mt-3 text-2xl font-semibold tabular-nums">{number(upstream.requests)}</div><div className="mt-1 text-xs text-muted-foreground">{tokens(upstream.totalTokens)} Token · {upstream.requests ? Math.round(100 * upstream.completed / upstream.requests) : 0}% {t('成功')}</div></div>)}{!selected?.upstreams.length && <p className="text-sm text-muted-foreground">{t('暂无请求日志')}</p>}</div></Section>
    <details className="mb-5 rounded-xl border bg-card p-4 text-sm"><summary className="cursor-pointer font-medium">{t('每日明细')}</summary><div className="mt-4 overflow-x-auto"><table className="w-full min-w-[640px]"><thead><tr>{['日期', '请求', '输入', '输出', '缓存', '总 Token'].map(label => <th key={label}>{t(label)}</th>)}</tr></thead><tbody>{(selected?.days || []).map(day => <tr key={day.date}><td>{day.date}</td><td>{number(day.requests)}</td><td>{tokens(day.inputTokens)}</td><td>{tokens(day.outputTokens)}</td><td>{tokens(day.cachedTokens)}</td><td>{tokens(day.totalTokens)}</td></tr>)}</tbody></table></div></details>
    <p className="mb-4 flex items-center gap-2 text-xs text-muted-foreground"><KeyRound className="size-3.5"/>{t('统计基于最近 30 天、最多 5000 条请求日志；速度为已完成请求的平均值。')}</p>
  </>
}
