package hashset

type Set[K comparable] map[K]struct{}

// New returns an empty int64 set
func New[K comparable]() Set[K] {
	return make(map[K]struct{})
}

// NewWithSize returns an empty int64 set initialized with specific size
func NewWithSize[K comparable](size int) Set[K] {
	return make(map[K]struct{}, size)
}

// Add adds the specified element to this set
// Always returns true due to the build-in map doesn't indicate caller whether the given element already exists
// Reserves the return type for future extension
func (s Set[K]) Add(value K) bool {
	s[value] = struct{}{}
	return true
}

// Contains returns true if this set contains the specified element
func (s Set[K]) Contains(value K) bool {
	if _, ok := s[value]; ok {
		return true
	}
	return false
}

// Remove removes the specified element from this set
// Always returns true due to the build-in map doesn't indicate caller whether the given element already exists
// Reserves the return type for future extension
func (s Set[K]) Remove(value K) bool {
	delete(s, value)
	return true
}

// Range calls f sequentially for each value present in the hashset.
// If f returns false, range stops the iteration.
func (s Set[K]) Range(f func(value K) bool) {
	for k := range s {
		if !f(k) {
			break
		}
	}
}

// Len returns the number of elements of this set
func (s Set[K]) Len() int {
	return len(s)
}
