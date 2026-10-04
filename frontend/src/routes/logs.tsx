import { createFileRoute } from '@tanstack/react-router'
import { Logs } from '../pages/Core'

export const Route = createFileRoute('/logs')({ component: Logs })
