package semantic

// ==========================================
// SYMBOL TABLE
// ==========================================

type Symbol struct {
	Name    string
	Type    string
	IsState bool
}

type Scope struct {
	Store  map[string]*Symbol
	Parent *Scope
}

func NewScope(parent *Scope) *Scope {
	return &Scope{Store: make(map[string]*Symbol), Parent: parent}
}

func (s *Scope) Define(sym *Symbol) { s.Store[sym.Name] = sym }

func (s *Scope) Resolve(name string) (*Symbol, bool) {
	sym, ok := s.Store[name]
	if !ok && s.Parent != nil {
		return s.Parent.Resolve(name)
	}
	return sym, ok
}
