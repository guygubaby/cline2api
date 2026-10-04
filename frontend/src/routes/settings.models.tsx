import { createFileRoute } from '@tanstack/react-router'
import { ModelsSettingsPage } from '../pages/Settings'

export const Route = createFileRoute('/settings/models')({ component: ModelsSettingsPage })
