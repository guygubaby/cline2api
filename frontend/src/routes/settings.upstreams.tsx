import { createFileRoute } from '@tanstack/react-router'
import { UpstreamsSettingsPage } from '../pages/Settings'

export const Route = createFileRoute('/settings/upstreams')({ component: UpstreamsSettingsPage })
