import { Chip, Tooltip } from '@mui/material'
import { useQuery } from '@tanstack/react-query'

import { api } from '../api/client'
import { useUi } from '../store/ui'

// HealthBadge is in the toolbar because it answers the first question an operator has when a panel looks
// wrong: is the problem the transfer, or is it that this page cannot reach the sender at all.
export function HealthBadge() {
  const refreshMs = useUi((state) => state.refreshMs)
  const { data, isError } = useQuery({
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
  return (
    <Tooltip title={`Protocol version ${data?.protocol_version ?? '—'}`}>
      <Chip size="small" color="success" variant="outlined" label={`v${data?.protocol_version ?? '—'}`} />
    </Tooltip>
  )
}
