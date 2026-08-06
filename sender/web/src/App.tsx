import { useMemo } from 'react'
import { Link as RouterLink, Navigate, Route, Routes, useLocation } from 'react-router-dom'
import {
  AppBar,
  Box,
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
import SendIcon from '@mui/icons-material/Send'

import { Dashboard } from './pages/Dashboard'
import { Display } from './pages/Display'
import { NewTransfer } from './pages/NewTransfer'
import { TransferDetail } from './pages/TransferDetail'
import { Transfers } from './pages/Transfers'
import { Profiles } from './pages/Profiles'
import { Settings } from './pages/Settings'
import { HealthBadge } from './components/HealthBadge'
import { useUi } from './store/ui'

const tabs = [
  { path: '/', label: 'Dashboard' },
  { path: '/send', label: 'Send a file' },
  { path: '/transfers', label: 'Transfers' },
  { path: '/display', label: 'Display' },
  { path: '/settings', label: 'Settings' },
  { path: '/profiles', label: 'Profiles' },
]

export function App() {
  const { theme: mode, setTheme } = useUi()
  const location = useLocation()

  const theme = useMemo(
    () =>
      createTheme({
        palette: {
          mode,
          // A dark interface by default, because this is a wall-mounted operations screen as often as a
          // desktop one, and a bright panel in a dim room is what an operator turns away from.
          primary: { main: mode === 'dark' ? '#7cc4ff' : '#0b5cad' },
          success: { main: '#3fbf7f' },
          warning: { main: '#e0a52e' },
          error: { main: '#e0574a' },
          background: mode === 'dark' ? { default: '#0d1117', paper: '#161b22' } : undefined,
        },
        shape: { borderRadius: 10 },
        typography: {
          fontFamily: '"Inter", system-ui, -apple-system, "Segoe UI", sans-serif',
          // Figures are read against each other down a column, so they are set in a monospaced face
          // wherever they appear: proportional digits make two numbers of the same magnitude look
          // different lengths.
          fontSize: 14,
        },
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
          <SendIcon color="primary" />
          <Typography variant="h6" sx={{ fontWeight: 600 }}>
            Optical Transport · Sender
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
        <Box>
          <Routes>
            <Route path="/" element={<Dashboard />} />
            <Route path="/send" element={<NewTransfer />} />
            <Route path="/transfers" element={<Transfers />} />
            <Route path="/transfers/:id" element={<TransferDetail />} />
            <Route path="/display" element={<Display />} />
            <Route path="/settings" element={<Settings />} />
            <Route path="/profiles" element={<Profiles />} />
            <Route path="*" element={<Navigate to="/" replace />} />
          </Routes>
        </Box>
      </Container>
    </ThemeProvider>
  )
}
