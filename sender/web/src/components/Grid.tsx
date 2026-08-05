import { Box, type SxProps, type Theme } from '@mui/material'
import type { ReactNode } from 'react'

// A twelve-column grid, built on CSS grid rather than on the component library's.
//
// The library's grid has been through several incompatible shapes across recent major versions — item/xs,
// then Grid2, then a size prop — and pinning the app to one of them means a routine dependency bump breaks
// the build for reasons that have nothing to do with this project. CSS grid does not move, so this is a dozen
// lines that will not need revisiting.
interface Size {
  xs?: number
  sm?: number
  md?: number
  lg?: number
}

export function Grid({
  container,
  size,
  spacing = 2,
  children,
  sx,
}: {
  container?: boolean
  size?: Size
  spacing?: number
  children?: ReactNode
  sx?: SxProps<Theme>
}) {
  if (container) {
    return (
      <Box sx={{ display: 'grid', gridTemplateColumns: 'repeat(12, 1fr)', gap: spacing, ...sx }}>
        {children}
      </Box>
    )
  }

  // A missing breakpoint inherits the one below it, which is how the library's grid behaves and what every
  // call site here assumes. Full width is the default, so a tile with no size given stacks rather than
  // collapsing to nothing.
  const span = (columns?: number) => (columns ? `span ${columns}` : undefined)
  return (
    <Box
      sx={{
        gridColumn: {
          xs: span(size?.xs ?? 12),
          sm: span(size?.sm),
          md: span(size?.md),
          lg: span(size?.lg),
        },
        minWidth: 0,
        ...sx,
      }}
    >
      {children}
    </Box>
  )
}
