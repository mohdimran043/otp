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

import { LiveCapture } from './pages/LiveCapture'
import { Transmissions } from './pages/Transmissions'
import { TransmissionDetail } from './pages/TransmissionDetail'
import { DecodeFailures } from './pages/DecodeFailures'
import { Settings } from './pages/Settings'
import { HealthBadge } from './components/HealthBadge'
import { useUi } from './store/ui'

const tabs = [
  { path: '/', label: 'Live capture' },
  { path: '/transmissions', label: 'Transmissions' },
  { path: '/failures', label: 'Decode failures' },
  // "Settings" rather than "Decoder": the page now configures the camera as well, and the camera is the
  // thing an operator goes looking for.
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
        <Toolbar sx={{ gap: 2 }}>
          <VideocamIcon color="primary" />
          <Typography variant="h6" sx={{ fontWeight: 600 }}>
            Optical Transport · Receiver
          </Typography>

          <Tabs value={current?.path ?? '/'} sx={{ ml: 3, flexGrow: 1 }}>
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
