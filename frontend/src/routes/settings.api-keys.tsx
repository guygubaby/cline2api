import { createFileRoute } from '@tanstack/react-router'
import { ApiKeysSettingsPage } from '../pages/Settings'

export const Route = createFileRoute('/settings/api-keys')({ component: ApiKeysSettingsPage })
