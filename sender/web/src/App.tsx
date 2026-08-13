import '@fontsource-variable/archivo'
import '@fontsource/martian-mono/400.css'
import '@fontsource/martian-mono/500.css'
import '@fontsource/martian-mono/600.css'

import { useMemo } from 'react'
import { Link as RouterLink, Navigate, Route, Routes, useLocation } from 'react-router-dom'
import {
  AppBar,
  Box,
  Container,
  CssBaseline,
  Tab,
  Tabs,
  ThemeProvider,
  Toolbar,
  Typography,
} from '@mui/material'
import SendIcon from '@mui/icons-material/Send'

import { Dashboard } from './pages/Dashboard'
import { Display } from './pages/Display'
import { instrument } from './theme'
import { NewTransfer } from './pages/NewTransfer'
import { TransferDetail } from './pages/TransferDetail'
import { Transfers } from './pages/Transfers'
import { Settings } from './pages/Settings'
import { HealthBadge } from './components/HealthBadge'

const tabs = [
  { path: '/', label: 'Dashboard' },
  { path: '/send', label: 'Send a file' },
  { path: '/transfers', label: 'Transfers' },
  { path: '/display', label: 'Display' },
  { path: '/settings', label: 'Settings' },
]

export function App() {
  const location = useLocation()

  const theme = useMemo(() => instrument(), [])

  const current = tabs.find((tab) =>
    tab.path === '/' ? location.pathname === '/' : location.pathname.startsWith(tab.path),
  )

  return (
    <ThemeProvider theme={theme}>
      <CssBaseline />
      <AppBar position="sticky" color="default" sx={{ borderBottom: 1, borderColor: 'divider' }}>
        <Toolbar sx={{ gap: { xs: 1, md: 2 }, px: { xs: 1, md: 3 } }}>
          <SendIcon color="primary" />
          {/* Shortened on a phone. At 390 pixels the full title wrapped onto three lines and pushed the tabs
              out of reach, so the page an operator arrived on was the only one they could get to. */}
          <Typography
            variant="h6"
            noWrap
            sx={{ fontWeight: 600, display: { xs: 'none', md: 'block' } }}
          >
            Optical Transport · Sender
          </Typography>
          <Typography
            variant="subtitle1"
            noWrap
            sx={{ fontWeight: 600, display: { xs: 'block', md: 'none' } }}
          >
            Sender
          </Typography>

          {/* Scrollable, because five tabs do not fit a phone and the ones that did not fit were simply
              unreachable — there was no indication they existed. */}
          <Tabs
            value={current?.path ?? '/'}
            variant="scrollable"
            scrollButtons="auto"
            allowScrollButtonsMobile
            sx={{ ml: { xs: 1, md: 3 }, flexGrow: 1, minWidth: 0 }}
          >
            {tabs.map((tab) => (
              <Tab key={tab.path} value={tab.path} label={tab.label} component={RouterLink} to={tab.path} />
            ))}
          </Tabs>

          <HealthBadge />
        </Toolbar>
      </AppBar>

      <Container maxWidth="xl" sx={{ py: 3 }}>
        <Box>
          <Routes>
            <Route path="/" element={<Dashboard />} />
            <Route path="/send" element={<NewTransfer />} />
            <Route path="/transfers" element={<Transfers />} />
            <Route path="/transfers/:id" element={<TransferDetail />} />
            <Route path="/display" element={<Display />} />
            <Route path="/settings" element={<Settings />} />
            <Route path="*" element={<Navigate to="/" replace />} />
          </Routes>
        </Box>
      </Container>
    </ThemeProvider>
  )
}
