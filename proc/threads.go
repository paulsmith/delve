package proc

import (
	"debug/gosym"
	"encoding/binary"
	"fmt"
	"path/filepath"

	sys "golang.org/x/sys/unix"

	"github.com/derekparker/delve/dwarf/frame"
	"github.com/derekparker/delve/source"
)

// Thread represents a single thread in the traced process
// Id represents the thread id or port, Process holds a reference to the
// Process struct that contains info on the process as
// a whole, and Status represents the last result of a `wait` call
// on this thread.
type Thread struct {
	Id                int             // Thread ID or mach port
	Status            *sys.WaitStatus // Status returned from last wait call
	CurrentBreakpoint *Breakpoint     // Breakpoint thread is currently stopped at

	dbp            *Process
	singleStepping bool
	running        bool
	os             *OSSpecificDetails
}

// Represents the location of a thread.
// Holds information on the current instruction
// address, the source file:line, and the function.
type Location struct {
	PC   uint64
	File string
	Line int
	Fn   *gosym.Func
}

// Continue the execution of this thread.
//
// If we are currently at a breakpoint, we'll clear it
// first and then resume execution. Thread will continue until
// it hits a breakpoint or is signaled.
func (thread *Thread) Continue() error {
	pc, err := thread.PC()
	if err != nil {
		return err
	}
	// Check whether we are stopped at a breakpoint, and
	// if so, single step over it before continuing.
	if bp, ok := thread.dbp.FindBreakpoint(pc); ok {
		if !bp.hardware {
			fmt.Println("single step past breakpoint")
			if err := thread.Step(); err != nil {
				return err
			}
		}
	}
	return thread.resume()
}

// Step a single instruction.
//
// Executes exactly one instruction and then returns.
// If the thread is at a breakpoint, we first clear it,
// execute the instruction, and then replace the breakpoint.
// Otherwise we simply execute the next instruction.
func (thread *Thread) Step() (err error) {
	thread.running = true
	thread.singleStepping = true
	defer func() {
		thread.singleStepping = false
		thread.running = false
	}()
	pc, err := thread.PC()
	if err != nil {
		return err
	}

	bp, ok := thread.dbp.Breakpoints[pc]
	if ok {
		// Clear the breakpoint so that we can continue execution.
		_, err = bp.Clear(thread)
		if err != nil {
			return err
		}

		// Restore breakpoint now that we have passed it.
		defer func() {
			if bp.hardware {
				err = thread.dbp.setHardwareBreakpoint(bp.reg, thread.Id, bp.Addr)
			} else {
				err = thread.dbp.writeSoftwareBreakpoint(thread, bp.Addr)
			}
		}()
	}

	err = thread.singleStep()
	if err != nil {
		return fmt.Errorf("step failed: %s", err.Error())
	}
	return nil
}

// Returns the threads location, including the file:line
// of the corresponding source code, the function we're in
// and the current instruction address.
func (thread *Thread) Location() (*Location, error) {
	pc, err := thread.PC()
	if err != nil {
		return nil, err
	}
	f, l, fn := thread.dbp.PCToLine(pc)
	return &Location{PC: pc, File: f, Line: l, Fn: fn}, nil
}

type ThreadBlockedError struct{}

func (tbe ThreadBlockedError) Error() string {
	return ""
}

// Set breakpoints for potential next lines.
//
// There are two modes of operation for this method. First,
// if we are executing Go code, we can use the stdlib AST
// information to determine which lines we could potentially
// end up at. Parsing the source file into an AST and traversing
// it lets us gain insight into whether we're at a branch, and
// where that branch could end up at, etc...
//
// However, if we are executing C code, we use the DWARF
// debug_line information and essentially set a breakpoint
// at every single line within the current function, and
// another at the functions return address, in case we're at
// the end.
func (thread *Thread) setNextBreakpoints() (err error) {
	if thread.blocked() {
		return ThreadBlockedError{}
	}
	curpc, err := thread.PC()
	if err != nil {
		return err
	}
	g, err := thread.GetG()
	if err != nil {
		return err
	}
	if g.DeferPC != 0 {
		if _, err = thread.dbp.SetTempBreakpoint(g.DeferPC); err != nil {
			return err
		}
	}

	// Grab info on our current stack frame. Used to determine
	// whether we may be stepping outside of the current function.
	fde, err := thread.dbp.frameEntries.FDEForPC(curpc)
	if err != nil {
		return err
	}

	// Get current file/line.
	loc, err := thread.Location()
	if err != nil {
		return err
	}
	if filepath.Ext(loc.File) == ".go" {
		err = thread.next(curpc, fde, loc.File, loc.Line)
	} else {
		err = thread.cnext(curpc, fde, loc.File)
	}
	return err
}

// Go routine is exiting.
type GoroutineExitingError struct {
	goid int
}

func (ge GoroutineExitingError) Error() string {
	return fmt.Sprintf("goroutine %d is exiting", ge.goid)
}

// Use the AST to determine potential next lines.
func (thread *Thread) next(curpc uint64, fde *frame.FrameDescriptionEntry, file string, line int) error {
	lines, err := thread.dbp.ast.NextLines(file, line)
	if err != nil {
		if _, ok := err.(source.NoNodeError); !ok {
			return err
		}
	}

	ret, err := thread.ReturnAddress()
	if err != nil {
		return err
	}

	pcs := make([]uint64, 0, len(lines))
	for i := range lines {
		pcs = append(pcs, thread.dbp.lineInfo.AllPCsForFileLine(file, lines[i])...)
	}

	var covered bool
	for i := range pcs {
		if fde.Cover(pcs[i]) {
			covered = true
			break
		}
	}

	if !covered {
		fn := thread.dbp.goSymTable.PCToFunc(ret)
		if fn != nil && fn.Name == "runtime.goexit" {
			g, err := thread.GetG()
			if err != nil {
				return err
			}
			return GoroutineExitingError{goid: g.Id}
		}
	}
	pcs = append(pcs, ret)
	return thread.setNextTempBreakpoints(curpc, pcs)
}

// Set a breakpoint at every reachable location, as well as the return address. Without
// the benefit of an AST we can't be sure we're not at a branching statement and thus
// cannot accurately predict where we may end up.
func (thread *Thread) cnext(curpc uint64, fde *frame.FrameDescriptionEntry, file string) error {
	pcs := thread.dbp.lineInfo.AllPCsBetween(fde.Begin(), fde.End(), file)
	ret, err := thread.ReturnAddress()
	if err != nil {
		return err
	}
	pcs = append(pcs, ret)
	return thread.setNextTempBreakpoints(curpc, pcs)
}

func (thread *Thread) setNextTempBreakpoints(curpc uint64, pcs []uint64) error {
	for i := range pcs {
		if pcs[i] == curpc || pcs[i] == curpc-1 {
			continue
		}
		if _, err := thread.dbp.SetTempBreakpoint(pcs[i]); err != nil {
			if _, ok := err.(BreakpointExistsError); !ok {
				return err
			}
		}
	}
	return nil
}

// Sets the PC for this thread.
func (thread *Thread) SetPC(pc uint64) error {
	regs, err := thread.Registers()
	if err != nil {
		return err
	}
	return regs.SetPC(thread, pc)
}

// Returns information on the G (goroutine) that is executing on this thread.
//
// The G structure for a thread is stored in thread local memory. Execute instructions
// that move the *G structure into a CPU register, and then grab
// the new registers and parse the G structure.
//
// We cannot simply use the allg linked list in order to find the M that represents
// the given OS thread and follow its G pointer because on Darwin mach ports are not
// universal, so our port for this thread would not map to the `id` attribute of the M
// structure. Also, when linked against libc, Go prefers the libc version of clone as
// opposed to the runtime version. This has the consequence of not setting M.id for
// any thread, regardless of OS.
//
// In order to get around all this craziness, we read the address of the G structure for
// the current thread from the thread local storage area.
func (thread *Thread) GetG() (g *G, err error) {
	regs, err := thread.Registers()
	if err != nil {
		return nil, err
	}

	gaddrbs, err := thread.readMemory(uintptr(regs.TLS()+thread.dbp.arch.GStructOffset()), thread.dbp.arch.PtrSize())
	if err != nil {
		return nil, err
	}
	gaddr := binary.LittleEndian.Uint64(gaddrbs)

	g, err = parseG(thread, gaddr, false)
	if err == nil {
		g.thread = thread
	}
	return
}

// Returns whether the thread is stopped at
// the operating system level. Actual implementation
// is OS dependant, look in OS thread file.
func (thread *Thread) Stopped() bool {
	return thread.stopped()
}

// Stops this thread from executing. Actual
// implementation is OS dependant. Look in OS
// thread file.
func (thread *Thread) Halt() (err error) {
	defer func() {
		if err == nil {
			thread.running = false
		}
	}()
	if thread.Stopped() {
		return
	}
	err = thread.halt()
	return
}
