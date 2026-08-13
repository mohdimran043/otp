import '@fontsource-variable/archivo'
import '@fontsource/martian-mono/400.css'
import '@fontsource/martian-mono/500.css'
import '@fontsource/martian-mono/600.css'

import { useMemo } from 'react'
import { Link as RouterLink, Navigate, Route, Routes, useLocation } from 'react-router-dom'
import {
  AppBar,
  Container,
  CssBaseline,
  Tab,
  Tabs,
  ThemeProvider,
  Toolbar,
  Typography,
} from '@mui/material'
import VideocamIcon from '@mui/icons-material/Videocam'

import { instrument } from './theme'
import { Camera } from './pages/Camera'
import { LiveCapture } from './pages/LiveCapture'
import { Transmissions } from './pages/Transmissions'
import { TransmissionDetail } from './pages/TransmissionDetail'
import { DecodeFailures } from './pages/DecodeFailures'
import { Settings } from './pages/Settings'
import { HealthBadge } from './components/HealthBadge'

const tabs = [
  { path: '/', label: 'Live capture' },
  // The camera has a tab of its own rather than a section inside Settings. Granting a browser access to a
  // camera is a prompt an operator has to answer, and a page that exists only to ask it can ask on arrival —
  // buried in Settings, the prompt only ever fired from a Start button most operators never reached.
  { path: '/camera', label: 'Camera' },
  { path: '/transmissions', label: 'Transmissions' },
  { path: '/failures', label: 'Decode failures' },
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
          <VideocamIcon color="primary" />
          {/* Shortened on a phone. At 390 pixels the full title wrapped onto three lines and pushed the tabs
              out of reach, so the page an operator arrived on was the only one they could get to. */}
          <Typography
            variant="h6"
            noWrap
            sx={{ fontWeight: 600, display: { xs: 'none', md: 'block' } }}
          >
            Optical Transport · Receiver
          </Typography>
          <Typography
            variant="subtitle1"
            noWrap
            sx={{ fontWeight: 600, display: { xs: 'block', md: 'none' } }}
          >
            Receiver
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
        <Routes>
          <Route path="/" element={<LiveCapture />} />
          <Route path="/camera" element={<Camera />} />
          <Route path="/transmissions" element={<Transmissions />} />
          <Route path="/transmissions/:id" element={<TransmissionDetail />} />
          <Route path="/failures" element={<DecodeFailures />} />
          <Route path="/settings" element={<Settings />} />
          <Route path="*" element={<Navigate to="/" replace />} />
        </Routes>
      </Container>
    </ThemeProvider>
  )
}
