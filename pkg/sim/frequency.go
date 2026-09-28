package sim

import "strconv"

// ClassCount is how many invocation-frequency classes an app can fall in
const ClassCount = 6

// ClassBounds are the inclusive upper bounds of the first five classes in
// invocations per replayed window; decades, so a week's mean inter-arrival
// runs from under six seconds in the top class to over sixteen hours in
// the bottom one
var ClassBounds = [ClassCount - 1]int{10, 100, 1000, 10000, 100000}

// ClassOf returns the frequency class of an app with the given invocation
// count over the replayed window.
func ClassOf(count int) int {
	for class, bound := range ClassBounds {
		if count <= bound {
			return class
		}
	}
	return ClassCount - 1
}

// ClassLabel names a class the way the result tables print it.
func ClassLabel(class int) string {
	if class == 0 {
		return "<=" + strconv.Itoa(ClassBounds[0])
	}
	if class >= ClassCount-1 {
		return ">" + strconv.Itoa(ClassBounds[ClassCount-2])
	}
	return strconv.Itoa(ClassBounds[class-1]+1) + "-" + strconv.Itoa(ClassBounds[class])
}
