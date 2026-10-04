import { createFileRoute } from '@tanstack/react-router'
import { ImportPage } from '../pages/Core'

export const Route = createFileRoute('/import')({ component: ImportPage })
