package neuralnet

import (
	"runtime"
	"sync"

	"github.com/unixpickle/autofunc"
	"github.com/unixpickle/num-analysis/linalg"
)

const defaultMaxBatchSize = 10

type gradResult struct {
	Grad  autofunc.Gradient
	RGrad autofunc.RGradient
}

// GradBatcher computes the gradients of Learners
// for batches of training samples.
//
// It is not safe to call a Batcher's methods
// from multiple Goroutines concurrently.
//
// After you use a GradBatcher with a given Learner,
// you should never use the same GradBatcher for
// a Learner with different parameters.
type GradBatcher struct {
	Learner LearnBatcher

	// CostFunc is the cost function used to compute
	// error values and the gradients of said values.
	CostFunc CostFunc

	// MaxGoroutines is the maximum number of Goroutines
	// the GradBatcher will use simultaneously.
	// If this is 0, a reasonable default is used.
	MaxGoroutines int

	// MaxBatchSize is the maximum number of samples the
	// GradBatcher will pass to the learner at once.
	// If this is 0, a reasonable default is used.
	MaxBatchSize int

	Cache *autofunc.VectorCache

	gradCache       gradientCache
	lastGradResult  autofunc.Gradient
	lastGradRResult autofunc.RGradient
}

// BatchGradient computes the error gradient for a
// batch of samples.
//
// The resulting gradient and its constituent vectors
// are only valid until the next call to BatchGradient
// or BatchRGradient, at which point the vectors may
// be re-used or overwritten.
func (b *GradBatcher) BatchGradient(s *SampleSet) autofunc.Gradient {
	grad, _ := b.batch(nil, s)
	return grad
}

// BatchRGradient computes the Gradient and RGradient
// for a batch of samples.
//
// The resulting values are  only valid until the next
// call to BatchGradient or BatchRGradient, when the
// vectors may be re-used.
func (b *GradBatcher) BatchRGradient(v autofunc.RVector, s *SampleSet) (autofunc.Gradient,
	autofunc.RGradient) {
	return b.batch(v, s)
}

func (b *GradBatcher) batch(rv autofunc.RVector, s *SampleSet) (autofunc.Gradient,
	autofunc.RGradient) {
	if b.gradCache.variables == nil {
		b.gradCache.variables = b.Learner.Parameters()
	} else {
		if b.lastGradResult != nil {
			b.gradCache.Free(b.lastGradResult)
		}
		if b.lastGradRResult != nil {
			b.gradCache.FreeR(b.lastGradRResult)
		}
	}

	maxGos := b.goroutineCount()
	maxBatch := b.batchSize()

	if maxGos < 2 || len(s.Inputs) <= maxBatch {
		b.lastGradResult, b.lastGradRResult = b.runBatches(rv, s)
		return b.lastGradResult, b.lastGradRResult
	}

	goCount := len(s.Inputs) / maxBatch
	if len(s.Inputs)%maxBatch != 0 {
		goCount++
	}
	if goCount > maxGos {
		goCount = maxGos
	}

	batchChan := make(chan *SampleSet, len(s.Inputs)/maxBatch+1)
	for i := 0; i < len(s.Inputs); i += maxBatch {
		bs := maxBatch
		if bs > len(s.Inputs)-i {
			bs = len(s.Inputs) - i
		}
		batchChan <- s.Subset(i, i+bs)
	}
	close(batchChan)

	resChan := b.launchGoroutines(rv, batchChan, goCount)

	b.lastGradResult = nil
	b.lastGradRResult = nil
	for res := range resChan {
		if b.lastGradResult == nil {
			b.lastGradResult = res.Grad
		} else {
			b.lastGradResult.Add(res.Grad)
			b.gradCache.Free(res.Grad)
		}
		if b.lastGradRResult == nil {
			b.lastGradRResult = res.RGrad
		} else {
			b.lastGradRResult.Add(res.RGrad)
			b.gradCache.FreeR(res.RGrad)
		}
	}

	return b.lastGradResult, b.lastGradRResult
}

func (b *GradBatcher) runBatches(rv autofunc.RVector, s *SampleSet) (autofunc.Gradient,
	autofunc.RGradient) {
	grad := b.gradCache.Alloc()
	var rgrad autofunc.RGradient
	if rv != nil {
		rgrad = b.gradCache.AllocR()
	}

	batchSize := b.batchSize()
	for i := 0; i < len(s.Inputs); i += batchSize {
		bs := batchSize
		if bs > len(s.Inputs)-i {
			bs = len(s.Inputs) - i
		}
		b.runBatch(rv, s.Subset(i, i+bs), grad, rgrad)
	}

	return grad, rgrad
}

func (b *GradBatcher) launchGoroutines(rv autofunc.RVector,
	in <-chan *SampleSet, goCount int) <-chan gradResult {
	resChan := make(chan gradResult)
	var wg sync.WaitGroup
	for i := 0; i < goCount; i++ {
		wg.Add(1)
		grad := b.gradCache.Alloc()
		var rgrad autofunc.RGradient
		if rv != nil {
			rgrad = b.gradCache.AllocR()
		}
		go func(grad autofunc.Gradient, rgrad autofunc.RGradient) {
			defer wg.Done()
			for batch := range in {
				b.runBatch(rv, batch, grad, rgrad)
			}
			resChan <- gradResult{grad, rgrad}
		}(grad, rgrad)
	}
	go func() {
		wg.Wait()
		close(resChan)
	}()
	return resChan
}

func (b *GradBatcher) runBatch(rv autofunc.RVector, s *SampleSet, grad autofunc.Gradient,
	rgrad autofunc.RGradient) {
	if len(s.Inputs) == 0 {
		return
	}
	sampleCount := len(s.Inputs)
	inputSize := len(s.Inputs[0])
	outputSize := len(s.Outputs[0])
	inVec := b.Cache.Alloc(sampleCount * inputSize)
	outVec := b.Cache.Alloc(sampleCount * outputSize)
	defer b.Cache.Free(inVec)
	defer b.Cache.Free(outVec)

	for i, in := range s.Inputs {
		copy(inVec[i*inputSize:], in)
	}
	for i, out := range s.Outputs {
		copy(outVec[i*outputSize:], out)
	}

	inVar := &autofunc.Variable{inVec}
	if rgrad != nil {
		rVar := autofunc.NewRVariable(inVar, rv)
		result := b.Learner.MakeRBatcher(b.Cache).BatchR(rv, rVar, sampleCount)
		cost := b.CostFunc.CostR(rv, outVec, result)
		cost.PropagateRGradient(linalg.Vector{1}, linalg.Vector{0},
			rgrad, grad)
		cost.Release()
	} else {
		result := b.Learner.MakeBatcher(b.Cache).Batch(inVar, sampleCount)
		cost := b.CostFunc.Cost(outVec, result)
		cost.PropagateGradient(linalg.Vector{1}, grad)
		cost.Release()
	}
}

func (b *GradBatcher) goroutineCount() int {
	max := runtime.GOMAXPROCS(0)
	if b.MaxGoroutines == 0 || b.MaxGoroutines > max {
		return max
	} else {
		return b.MaxGoroutines
	}
}

func (b *GradBatcher) batchSize() int {
	if b.MaxBatchSize != 0 {
		return b.MaxBatchSize
	} else {
		return defaultMaxBatchSize
	}
}

type gradientCache struct {
	variables  []*autofunc.Variable
	gradients  []autofunc.Gradient
	rGradients []autofunc.RGradient
}

func (g *gradientCache) Alloc() autofunc.Gradient {
	if len(g.gradients) == 0 {
		res := autofunc.NewGradient(g.variables)
		return res
	}
	res := g.gradients[len(g.gradients)-1]
	g.gradients = g.gradients[:len(g.gradients)-1]
	res.Zero()
	return res
}

func (g *gradientCache) AllocR() autofunc.RGradient {
	if len(g.rGradients) == 0 {
		res := autofunc.NewRGradient(g.variables)
		return res
	}
	res := g.rGradients[len(g.rGradients)-1]
	g.rGradients = g.rGradients[:len(g.rGradients)-1]
	res.Zero()
	return res
}

func (g *gradientCache) Free(gr autofunc.Gradient) {
	g.gradients = append(g.gradients, gr)
}

func (g *gradientCache) FreeR(gr autofunc.RGradient) {
	g.rGradients = append(g.rGradients, gr)
}