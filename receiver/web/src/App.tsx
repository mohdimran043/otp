import { useMemo } from 'react'
import { Link as RouterLink, Navigate, Route, Routes, useLocation } from 'react-router-dom'
import {
  AppBar,
  Container,
  CssBaseline,
  IconButton,
  Tab,
  Tabs,
  ThemeProvider,
  Toolbar,
  Tooltip,
  Typography,
  createTheme,
} from '@mui/material'
import DarkModeIcon from '@mui/icons-material/DarkMode'
import LightModeIcon from '@mui/icons-material/LightMode'
import VideocamIcon from '@mui/icons-material/Videocam'

import { Camera } from './pages/Camera'
import { LiveCapture } from './pages/LiveCapture'
import { Transmissions } from './pages/Transmissions'
import { TransmissionDetail } from './pages/TransmissionDetail'
import { DecodeFailures } from './pages/DecodeFailures'
import { Settings } from './pages/Settings'
import { HealthBadge } from './components/HealthBadge'
import { useUi } from './store/ui'

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
  const { theme: mode, setTheme } = useUi()
  const location = useLocation()

  const theme = useMemo(
    () =>
      createTheme({
        palette: {
          mode,
          primary: { main: mode === 'dark' ? '#9ad0a0' : '#1f7a3d' },
          success: { main: '#3fbf7f' },
          warning: { main: '#e0a52e' },
          error: { main: '#e0574a' },
          background: mode === 'dark' ? { default: '#0d1117', paper: '#161b22' } : undefined,
        },
        shape: { borderRadius: 10 },
        typography: { fontFamily: '"Inter", system-ui, -apple-system, "Segoe UI", sans-serif', fontSize: 14 },
        components: {
          MuiPaper: { defaultProps: { elevation: 0 }, styleOverrides: { root: { backgroundImage: 'none' } } },
          MuiTableCell: { styleOverrides: { root: { fontVariantNumeric: 'tabular-nums' } } },
        },
      }),
    [mode],
  )

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
          <Tooltip title={mode === 'dark' ? 'Switch to light' : 'Switch to dark'}>
            <IconButton onClick={() => setTheme(mode === 'dark' ? 'light' : 'dark')}>
              {mode === 'dark' ? <LightModeIcon /> : <DarkModeIcon />}
            </IconButton>
          </Tooltip>
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
