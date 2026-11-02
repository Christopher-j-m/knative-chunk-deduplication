package sim

import "strconv"

// Order the simulated apps into six groups/classes based on how frequent they were called
// during the simulation, so we can make results based on frequency groups.
// Each class is roughly ten times busier/more frequently called than the ones in the group below
const ClassCount = 6

// Frequency classes, that marks the boundaries of each, e.g. an app with 1001 is in the <= 10000 group,
// while we only have only 5 groups with cut-offs, since everything above the 100000 group is automatically in the last one
var ClassBounds = [ClassCount - 1]int{10, 100, 1000, 10000, 100000}

// Based on how many times a app was called, this function returns in which group it lands
func ClassOf(appCallCount int) int {
	for class, bound := range ClassBounds {
		if appCallCount <= bound {
			return class
		}
	}
	return ClassCount - 1
}

// Helper function for printing the range of the specific class
// => class = 3, returns "1001-10000"
func ClassLabel(classNumber int) string {
	if classNumber == 0 {
		return "<=" + strconv.Itoa(ClassBounds[0])
	}
	if classNumber >= ClassCount-1 {
		return ">" + strconv.Itoa(ClassBounds[ClassCount-2])
	}
	return strconv.Itoa(ClassBounds[classNumber-1]+1) + "-" + strconv.Itoa(ClassBounds[classNumber])
}
