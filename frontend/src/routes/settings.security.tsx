import { createFileRoute } from '@tanstack/react-router'
import { SecuritySettingsPage } from '../pages/Settings'

export const Route = createFileRoute('/settings/security')({ component: SecuritySettingsPage })
