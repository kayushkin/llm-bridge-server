package server

// withServicesComposed composes the split-out packages of a Server a test
// built by hand from its fields, as New does, and returns it.
func (s *Server) withServicesComposed() *Server {
	s.composeServices()
	return s
}
