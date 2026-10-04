import { createFileRoute } from '@tanstack/react-router'
import { AdvancedSettingsPage } from '../pages/Settings'

export const Route = createFileRoute('/settings/advanced')({ component: AdvancedSettingsPage })
