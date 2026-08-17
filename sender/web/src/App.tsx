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
  IconButton,
  Toolbar,
  Tooltip,
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
import DarkModeIcon from '@mui/icons-material/DarkMode'
import LightModeIcon from '@mui/icons-material/LightMode'

import { useUi } from './store/ui'

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

  // The theme follows the operator's choice, which the store persists. Rebuilt only when the mode
  // changes, so a toggle re-themes the tree and nothing else does.
  const mode = useUi((state) => state.theme)
  const setTheme = useUi((state) => state.setTheme)
  const theme = useMemo(() => instrument(mode), [mode])

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

          {/* Dark is still the default and the right choice at a camera: the operator is facing a bright
              panel in a dim room, and a pale interface beside it ruins their dark adaptation. Light is for
              everywhere else this gets used — a desk, a projector, a screenshot in a document. */}
          <Tooltip title={mode === 'dark' ? 'Switch to the light theme' : 'Switch to the dark theme'}>
            <IconButton
              size="small"
              onClick={() => setTheme(mode === 'dark' ? 'light' : 'dark')}
              aria-label={mode === 'dark' ? 'Switch to the light theme' : 'Switch to the dark theme'}
            >
              {mode === 'dark' ? <LightModeIcon fontSize="small" /> : <DarkModeIcon fontSize="small" />}
            </IconButton>
          </Tooltip>
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
