import { Chip, Tooltip } from '@mui/material'
import { useQuery } from '@tanstack/react-query'

import { api } from '../api/client'
import { useUi } from '../store/ui'

// HealthBadge is in the toolbar because it answers the first question an operator has when a panel looks
// wrong: is the problem the transfer, or is it that this page cannot reach the sender at all.
export function HealthBadge() {
  const refreshMs = useUi((state) => state.refreshMs)
  // Still polled: the query failing is what turns the badge red, and that is the whole of its job.
  const { isError } = useQuery({
    queryKey: ['health'],
    queryFn: api.health,
    refetchInterval: Math.max(refreshMs, 2000),
  })

  if (isError) {
    return (
      <Tooltip title="The sender is not answering. Nothing on this page is current.">
        <Chip size="small" color="error" label="unreachable" />
      </Tooltip>
    )
  }
  // The badge says whether the service is answering, which is the only thing a badge in a header can
  // usefully say. It used to read "v1" — the protocol version, which never changes, is not something an
  // operator acts on, and read as a build number to everyone who saw it. It remains available where a
  // number that never moves belongs — /health reports it, and the receiver shows it on its Settings page.
  return (
    <Tooltip title="This service is answering, and the figures on this page are current.">
      <Chip size="small" color="success" variant="outlined" label="connected" />
    </Tooltip>
  )
}
