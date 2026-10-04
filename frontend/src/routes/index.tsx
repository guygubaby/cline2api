import { createFileRoute } from '@tanstack/react-router'
import { Dashboard } from '../pages/Dashboard'

const ranges = ['today', '1d', '7d', '14d', '30d', 'all'] as const
export const Route = createFileRoute('/')({
  validateSearch: (search: Record<string, unknown>): { range?: typeof ranges[number] } => ({ range: ranges.includes(search.range as typeof ranges[number]) ? search.range as typeof ranges[number] : 'today' }),
  component: Dashboard,
})
