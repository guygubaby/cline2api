import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { createRoot } from 'react-dom/client'
import { RouterProvider, createRouter } from '@tanstack/react-router'
import { routeTree } from './routeTree.gen'
import './styles.css'

const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false, refetchOnWindowFocus: false } } })
const router = createRouter({ routeTree, basepath: '/admin' })
declare module '@tanstack/react-router' { interface Register { router: typeof router } }

const oldPage = location.hash.slice(1)
if (['dashboard', 'accounts', 'import', 'logs', 'model-visibility', 'providers', 'settings', 'about'].includes(oldPage)) {
  history.replaceState(null, '', `/admin/${oldPage === 'dashboard' ? '' : oldPage}${location.search}`)
}
createRoot(document.getElementById('root')!).render(
  <QueryClientProvider client={queryClient}>
    <RouterProvider router={router} />
  </QueryClientProvider>,
)
