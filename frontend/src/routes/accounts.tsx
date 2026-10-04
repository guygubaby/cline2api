import { createFileRoute } from '@tanstack/react-router'
import { Accounts } from '../pages/Core'

export const Route = createFileRoute('/accounts')({ component: Accounts })
