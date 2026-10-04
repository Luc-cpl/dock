import React from 'react'
import ReactDOM from 'react-dom/client'
import { CssBaseline, ThemeProvider, createTheme } from '@mui/material'
import App from './App'

const theme = createTheme({
  palette: {
    mode: 'light',
    primary: { main: '#4f46e5', dark: '#3730a3', light: '#eef2ff' },
    background: { default: '#f7f8fc', paper: '#fff' },
    success: { main: '#059669' },
    warning: { main: '#d97706' },
    error: { main: '#dc2626' },
    text: { primary: '#182033', secondary: '#667085' },
  },
  shape: { borderRadius: 12 },
  typography: {
    fontFamily: 'Inter, ui-sans-serif, system-ui, -apple-system, BlinkMacSystemFont, "Segoe UI", sans-serif',
    h1: { fontSize: '1.7rem', fontWeight: 700, letterSpacing: '-0.035em' },
    h2: { fontSize: '1.15rem', fontWeight: 650, letterSpacing: '-0.02em' },
    button: { textTransform: 'none', fontWeight: 600 },
  },
  components: {
    MuiPaper: { styleOverrides: { root: { backgroundImage: 'none', border: '1px solid #e8eaf1', boxShadow: '0 2px 8px rgba(20, 28, 50, .035)' } } },
    MuiButton: { defaultProps: { disableElevation: true }, styleOverrides: { root: { borderRadius: 9, paddingInline: 14 } } },
    MuiTableCell: { styleOverrides: { head: { color: '#778097', fontSize: 11, fontWeight: 700, letterSpacing: '.06em', textTransform: 'uppercase' } } },
  },
})

ReactDOM.createRoot(document.getElementById('root')!).render(
  <React.StrictMode>
    <ThemeProvider theme={theme}>
      <CssBaseline />
      <App />
    </ThemeProvider>
  </React.StrictMode>,
)
