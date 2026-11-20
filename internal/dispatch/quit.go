package dispatch

// PrepareQuit closes the spawn gate and reports whether launched jobs remain.
// A job stays active until its container's removal has been confirmed.
func (c *Controller) PrepareQuit(stop bool) (active bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.draining = true
	for _, r := range c.runners {
		active = active || r.launched
		if stop || !r.launched {
			r.cancel()
		}
	}
	return active
}

func (c *Controller) Resume() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.draining = false
	c.signal()
}

// A quit can arrive while the service is still inventorying previous jobs.
// Publish the controller only after applying the supervisor's current intent.
func (h *Health) attach(c *Controller) {
	h.quitMu.Lock()
	defer h.quitMu.Unlock()
	if h.draining {
		c.PrepareQuit(h.stopping)
	}
	h.controller.Store(c)
}

func (h *Health) PrepareQuit(stop bool) (active, attached bool) {
	h.quitMu.Lock()
	defer h.quitMu.Unlock()
	h.draining = true
	h.stopping = h.stopping || stop
	if c := h.Controller(); c != nil {
		return c.PrepareQuit(h.stopping), true
	}
	return false, false
}

func (h *Health) Resume() {
	h.quitMu.Lock()
	defer h.quitMu.Unlock()
	h.draining, h.stopping = false, false
	if c := h.Controller(); c != nil {
		c.Resume()
	}
}
