import { BrowserRouter, Navigate, Route, Routes } from 'react-router-dom'
import { Shell } from './components/Shell'
import { CreateCustomerPage } from './pages/CreateCustomerPage'
import { CustomerDetailPage } from './pages/CustomerDetailPage'
import { CustomerListPage } from './pages/CustomerListPage'
import './styles.css'

export default function App() {
  return (
    <BrowserRouter>
      <Shell>
        <Routes>
          <Route path="/" element={<CustomerListPage />} />
          <Route path="/new" element={<CreateCustomerPage />} />
          <Route path="/customers/:id" element={<CustomerDetailPage />} />
          <Route path="*" element={<Navigate to="/" replace />} />
        </Routes>
      </Shell>
    </BrowserRouter>
  )
}
