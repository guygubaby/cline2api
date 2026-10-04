import { createFileRoute } from '@tanstack/react-router'
import { SettingsHome } from '../pages/Settings'

export const Route = createFileRoute('/settings/')({ component: SettingsHome })
