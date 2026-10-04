import { createFileRoute } from '@tanstack/react-router'
import { GeneralSettingsPage } from '../pages/Settings'

export const Route = createFileRoute('/settings/general')({ component: GeneralSettingsPage })
