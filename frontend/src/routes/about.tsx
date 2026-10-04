import { createFileRoute } from '@tanstack/react-router'
import { About } from '../pages/Settings'

export const Route = createFileRoute('/about')({ component: About })
