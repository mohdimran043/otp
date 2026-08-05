import { Chip, Tooltip } from '@mui/material'
import { useQuery } from '@tanstack/react-query'

import { api } from '../api/client'
import { useUi } from '../store/ui'

export function HealthBadge() {
  const refreshMs = useUi((state) => state.refreshMs)
  const { data, isError } = useQuery({
    queryKey: ['health'],
    queryFn: api.health,
    refetchInterval: Math.max(refreshMs, 2000),
  })

  if (isError) {
    return (
      <Tooltip title="The receiver is not answering. Nothing on this page is current.">
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
