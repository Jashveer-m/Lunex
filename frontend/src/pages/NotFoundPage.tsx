import { PageContainer } from '../components/Shell'
import { EmptyState } from '../components/ui'
import { Link } from '../lib/router'

export function NotFoundPage() {
  return (
    <PageContainer>
      <EmptyState title="Nothing here">
        <Link to="/" className="text-accent hover:underline">
          Back to today →
        </Link>
      </EmptyState>
    </PageContainer>
  )
}
