import { createContext, Suspense, useContext, useEffect, useState, type FormEvent, type ReactNode } from 'react'
import { Link, Outlet, useRouterState } from '@tanstack/react-router'
import { Activity, BookOpen, ChartNoAxesCombined, ChevronDown, Eye, EyeOff, KeyRound, Languages, Layers3, ListFilter, LogOut, Settings2, Upload, Users } from 'lucide-react'
import { useQuery, useQueryClient } from '@tanstack/react-query'
import { AnimatePresence, motion, useReducedMotion } from 'motion/react'
import { get, post } from './api'
import translations from './i18n.json'
import { Button as UiButton } from './components/ui/button'
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from './components/ui/card'
import { Input } from './components/ui/input'
import { Label } from './components/ui/label'
import { Separator } from './components/ui/separator'
import { DropdownMenu, DropdownMenuContent, DropdownMenuItem, DropdownMenuLabel, DropdownMenuSeparator, DropdownMenuTrigger } from './components/ui/dropdown-menu'
import { Sidebar, SidebarContent, SidebarFooter, SidebarGroup, SidebarGroupContent, SidebarGroupLabel, SidebarHeader, SidebarInset, SidebarMenu, SidebarMenuButton, SidebarMenuItem, SidebarMenuSub, SidebarMenuSubButton, SidebarMenuSubItem, SidebarProvider, SidebarTrigger, useSidebar } from './components/ui/sidebar'
import { TooltipProvider } from './components/ui/tooltip'

type Page = 'dashboard' | 'accounts' | 'import' | 'logs' | 'model-visibility' | 'providers' | 'settings' | 'about'
type AdminUser = { id: string; email: string }
type AdminContext = { t: (s: string) => string; notify: (s: string, error?: boolean) => void; locale: string; user?: AdminUser; run: (action: () => Promise<unknown>, success?: string) => Promise<boolean> }
const Context = createContext<AdminContext | null>(null)
export function useAdmin() { const value = useContext(Context); if (!value) throw Error('Admin context missing'); return value }
export const number = (n?: number) => new Intl.NumberFormat().format(n || 0)
export const tokens = (n?: number) => { const value = n || 0; for (const [unit, size] of [['B', 1e9], ['M', 1e6], ['K', 1e3]] as const) if (value >= size) return `${+(value / size).toFixed(1)}${unit}`; return String(value) }
export const duration = (ms?: number) => !ms || ms <= 0 ? '-' : ms < 1000 ? `${ms}ms` : `${(ms / 1000).toFixed(1)}s`
export function Section({ title, children, aside }: { title: string; children: ReactNode; aside?: ReactNode }) { return <Card className="mb-5 gap-0 overflow-hidden py-0"><CardHeader className="flex min-h-14 flex-row items-center justify-between gap-3 border-b py-3"><CardTitle className="text-sm font-semibold">{title}</CardTitle>{aside}</CardHeader><CardContent className="py-5">{children}</CardContent></Card> }
export function Field({ label, children }: { label: string; children: ReactNode }) { return <label className="flex min-w-0 flex-col gap-2 text-sm font-medium"><span>{label}</span>{children}</label> }
export function Button({ children, onClick, danger, disabled, type = 'button' }: { children: ReactNode; onClick?: () => void; danger?: boolean; disabled?: boolean; type?: 'button' | 'submit' }) { return <UiButton type={type} variant={danger ? 'destructive' : 'default'} disabled={disabled} onClick={onClick}>{children}</UiButton> }
export function PageTitle({ title, subtitle, action }: { title: string; subtitle?: string; action?: ReactNode }) { return <div className="mb-6 flex flex-wrap items-center justify-between gap-4"><div><h1 tabIndex={-1} className="text-2xl font-semibold tracking-tight focus:outline-none">{title}</h1>{subtitle && <p className="mt-1 text-sm text-muted-foreground">{subtitle}</p>}</div>{action}</div> }

const nav: { id: Page; to: '/' | '/accounts' | '/import' | '/logs' | '/model-visibility' | '/providers' | '/settings' | '/about'; label: string; icon: typeof Activity }[] = [
  { id: 'dashboard', to: '/', label: '概览', icon: ChartNoAxesCombined }, { id: 'accounts', to: '/accounts', label: '账号管理', icon: Users },
  { id: 'import', to: '/import', label: '导入账号', icon: Upload }, { id: 'logs', to: '/logs', label: '请求日志', icon: Activity },
  { id: 'model-visibility', to: '/model-visibility', label: '模型展示', icon: ListFilter }, { id: 'providers', to: '/providers', label: '第三方渠道', icon: Layers3 },
  { id: 'settings', to: '/settings', label: '设置', icon: Settings2 }, { id: 'about', to: '/about', label: '关于', icon: BookOpen },
]
const settingsNav = [
  { to: '/settings/general', label: '常规设置' }, { to: '/settings/api-keys', label: 'API 密钥' },
  { to: '/settings/security', label: '管理员与安全' }, { to: '/settings/models', label: '模型配置' },
  { to: '/settings/upstreams', label: '上游配置' }, { to: '/settings/advanced', label: '高级设置' },
] as const
function readLang() { try { const stored = localStorage.getItem('cline_admin_lang'); if (stored === 'en' || stored === 'zh') return stored } catch { /* browser storage disabled */ } return navigator.language.toLowerCase().startsWith('zh') ? 'zh' : 'en' }

function Navigation({ page, routePath, t }: { page: Page; routePath: string; t: (s: string) => string }) {
  const sidebar = useSidebar()
  const groups = [
    { label: '工作台', items: nav.slice(0, 1) },
    { label: '管理', items: nav.slice(1, 6) },
    { label: '系统', items: nav.slice(6) },
  ]
  return <Sidebar collapsible="icon">
    <SidebarHeader className="border-b p-3 group-data-[collapsible=icon]:p-2"><SidebarMenu><SidebarMenuItem><SidebarMenuButton size="lg" asChild><Link to="/" aria-label="Cline Proxy"><span className="flex size-8 shrink-0 items-center justify-center rounded-lg bg-primary text-primary-foreground"><KeyRound className="size-4"/></span><span className="flex min-w-0 flex-col group-data-[collapsible=icon]:hidden"><span className="truncate font-semibold">Cline Proxy</span><span className="text-xs text-muted-foreground">Admin Console</span></span></Link></SidebarMenuButton></SidebarMenuItem></SidebarMenu></SidebarHeader>
    <SidebarContent>{groups.map(group => <SidebarGroup key={group.label}><SidebarGroupLabel>{t(group.label)}</SidebarGroupLabel><SidebarGroupContent><SidebarMenu>{group.items.map(({ id, to, label, icon: Icon }) => <SidebarMenuItem key={id}><SidebarMenuButton asChild isActive={page === id} tooltip={t(label)}><Link to={to} aria-current={routePath === to ? 'page' : undefined} onClick={() => { if (sidebar.isMobile) sidebar.setOpenMobile(false) }}><Icon aria-hidden="true"/><span>{t(label)}</span></Link></SidebarMenuButton>{id === 'settings' && page === 'settings' && <SidebarMenuSub>{settingsNav.map(item => <SidebarMenuSubItem key={item.to}><SidebarMenuSubButton asChild isActive={routePath === item.to}><Link to={item.to} aria-current={routePath === item.to ? 'page' : undefined} onClick={() => { if (sidebar.isMobile) sidebar.setOpenMobile(false) }}>{t(item.label)}</Link></SidebarMenuSubButton></SidebarMenuSubItem>)}</SidebarMenuSub>}</SidebarMenuItem>)}</SidebarMenu></SidebarGroupContent></SidebarGroup>)}</SidebarContent>
    <SidebarFooter className="border-t p-3 group-data-[collapsible=icon]:hidden"><div className="flex items-center gap-2 rounded-lg px-2 py-1.5 text-xs text-muted-foreground"><span className="size-2 rounded-full bg-emerald-500"/> Go API</div></SidebarFooter>
  </Sidebar>
}

export function App() {
  const pathname = useRouterState({ select: state => state.location.pathname })
  const routePath = pathname.replace(/^\/admin(?=\/|$)/, '') || '/'
  const page = routePath.startsWith('/settings/') ? 'settings' : nav.find(item => item.to === routePath)?.id || 'dashboard'
  const settingsPage = settingsNav.find(item => item.to === routePath)
  const [lang, setLang] = useState(readLang)
  const [toast, setToast] = useState<{ message: string; error: boolean } | null>(null)
  const [login, setLogin] = useState(false)
  const [email, setEmail] = useState('')
  const [password, setPassword] = useState('')
  const [passwordVisible, setPasswordVisible] = useState(false)
  const [loginError, setLoginError] = useState('')
  const queryClient = useQueryClient()
  const reducedMotion = useReducedMotion()
  const t = (s: string) => lang === 'en' ? (translations as Record<string, string>)[s] || s : s
  const auth = useQuery({ queryKey: ['admin-me'], queryFn: async () => (await get<AdminUser>('me')).data })
  const notify = (message: string, error = false) => setToast({ message, error })
  const run = async (action: () => Promise<unknown>, success?: string) => { try { await action(); if (success) notify(t(success)); await queryClient.invalidateQueries(); return true } catch (error) { notify(`${t('操作失败')}: ${(error as Error).message}`, true); return false } }
  useEffect(() => { const onAuth = () => setLogin(true); addEventListener('admin-auth-required', onAuth); return () => removeEventListener('admin-auth-required', onAuth) }, [])
  useEffect(() => { document.documentElement.lang = lang === 'en' ? 'en' : 'zh-CN'; document.title = lang === 'en' ? 'Cline Proxy Admin' : 'Cline 代理管理面板'; try { localStorage.setItem('cline_admin_lang', lang); document.cookie = `cline_admin_lang=${lang};path=/admin;max-age=31536000` } catch { /* storage disabled */ } }, [lang])
  useEffect(() => { if (!toast) return; const timer = setTimeout(() => setToast(null), 3500); return () => clearTimeout(timer) }, [toast])
  async function submitLogin(event: FormEvent) { event.preventDefault(); try { await post('login', { email, password }); const me = await auth.refetch(); if (me.error) throw me.error; setLogin(false); setPassword(''); setPasswordVisible(false); setLoginError(''); await queryClient.invalidateQueries() } catch (error) { setLoginError((error as Error).message); setPassword(''); setPasswordVisible(false) } }
  async function logout() { try { await post('logout'); queryClient.clear(); setLogin(true); setPassword(''); setPasswordVisible(false); notify(t('已退出登录')) } catch (error) { notify((error as Error).message, true) } }
  const context = { t, notify, locale: lang === 'en' ? 'en-US' : 'zh-CN', user: auth.data, run }
  return <Context.Provider value={context}><TooltipProvider><SidebarProvider>
    {auth.isPending && !login ? <main className="grid min-h-svh w-full place-items-center text-sm text-muted-foreground">{t('加载中…')}</main> : login || auth.isError ? <main className="grid min-h-svh w-full place-items-center bg-muted/40 p-4"><motion.div initial={reducedMotion ? false : { opacity: 0, y: 12 }} animate={{ opacity: 1, y: 0 }} transition={{ duration: 0.24 }} className="w-full max-w-sm"><Card><CardHeader><div className="mb-3 flex size-10 items-center justify-center rounded-xl bg-primary text-primary-foreground"><KeyRound size={20}/></div><CardTitle className="text-xl">Cline Proxy</CardTitle><CardDescription>{t('管理后台登录')}</CardDescription></CardHeader><CardContent><form onSubmit={submitLogin} className="space-y-4"><div className="grid gap-2"><Label htmlFor="admin-email">{t('邮箱')}</Label><Input id="admin-email" autoFocus type="email" autoComplete="username" value={email} onChange={e => setEmail(e.target.value)} required/></div><div className="grid gap-2"><Label htmlFor="admin-password">{t('密码')}</Label><div className="relative"><Input id="admin-password" type={passwordVisible ? 'text' : 'password'} autoComplete="current-password" value={password} onChange={e => setPassword(e.target.value)} className="pr-11" required/><UiButton type="button" variant="ghost" size="icon-sm" className="absolute inset-y-0 right-1 my-auto text-muted-foreground hover:text-foreground" aria-label={t(passwordVisible ? '隐藏密码' : '显示密码')} aria-pressed={passwordVisible} onClick={() => setPasswordVisible(visible => !visible)}>{passwordVisible ? <EyeOff aria-hidden="true"/> : <Eye aria-hidden="true"/>}</UiButton></div></div>{loginError && <p role="alert" className="text-sm text-destructive">{loginError}</p>}<UiButton type="submit" className="w-full">{t('登录')}</UiButton></form></CardContent></Card></motion.div></main> : <>
      <Navigation page={page} routePath={routePath} t={t}/>
      <SidebarInset className="min-w-0 bg-muted/30"><header className="sticky top-0 z-20 flex h-14 shrink-0 items-center gap-3 border-b bg-background px-4 md:px-6"><SidebarTrigger aria-label={t('切换菜单')}/><Separator orientation="vertical" className="h-5"/><div className="min-w-0 text-sm text-muted-foreground">{t('管理后台')} <span className="mx-2">/</span> <span className="font-medium text-foreground">{t(nav.find(n => n.id === page)?.label || '概览')}</span>{settingsPage && <><span className="mx-2">/</span><span className="font-medium text-foreground">{t(settingsPage.label)}</span></>}</div><div className="ml-auto"><DropdownMenu><DropdownMenuTrigger asChild><UiButton variant="ghost" className="gap-2"><span className="flex size-7 items-center justify-center rounded-full bg-primary text-xs text-primary-foreground">{auth.data?.email?.[0]?.toUpperCase() || 'A'}</span><span className="hidden sm:inline">{auth.data?.email || 'Admin'}</span><ChevronDown className="size-3"/></UiButton></DropdownMenuTrigger><DropdownMenuContent align="end" className="w-56"><DropdownMenuLabel className="truncate">{auth.data?.email || 'Admin'}</DropdownMenuLabel><DropdownMenuSeparator/><DropdownMenuItem onSelect={() => setLang(lang === 'zh' ? 'en' : 'zh')}><Languages/>{lang === 'zh' ? 'English' : '中文'}</DropdownMenuItem><DropdownMenuSeparator/><DropdownMenuItem onSelect={logout}><LogOut/>{t('退出登录')}</DropdownMenuItem></DropdownMenuContent></DropdownMenu></div></header>
      <main id="mainContent" className="min-w-0 flex-1 p-4 md:p-6 lg:p-8"><AnimatePresence mode="wait" initial={false}><motion.div key={pathname} initial={reducedMotion ? false : { opacity: 0, y: 8 }} animate={{ opacity: 1, y: 0 }} exit={reducedMotion ? { opacity: 1 } : { opacity: 0, y: -4 }} transition={{ duration: reducedMotion ? 0 : 0.18 }} className="mx-auto w-full max-w-[1440px]"><Suspense fallback={<div className="p-8 text-sm text-muted-foreground">{t('加载中…')}</div>}><Outlet/></Suspense></motion.div></AnimatePresence></main></SidebarInset>
    </>}
    {toast && <div id="toast" className={`toast show ${toast.error ? 'error' : 'success'}`} role={toast.error ? 'alert' : 'status'} aria-live="polite">{toast.message}</div>}
  </SidebarProvider></TooltipProvider></Context.Provider>
}
