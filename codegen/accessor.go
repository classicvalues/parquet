package codegen

import "fmt"

func generateSliceAccessor(sess *session, nodes Nodes) error {

	leaf := nodes.Leaf()
	params := leaf.NewParams()
	params.SetIndent(4 * leaf.Depth)
	leafSnippet, err := expandAccessorMutatorTemlate(sliceReadLeaf, params)
	if err != nil {
		return err
	}
	childSnippet := leafSnippet
	for i := len(nodes) - 2; i >= 0; i-- {
		node := nodes[i]
		params = node.NewParams()
		params.SetIndent(4 * node.Depth)
		params.ChildSnippet = childSnippet
		params.ChildName = nodes[i+1].Field.Name
		//if node.Field.IsSlice {
			if childSnippet, err = expandAccessorMutatorTemlate(sliceReadNode, params); err != nil {
				return err
			}
		//}
	}
	root := nodes[0]
	rootParams := root.NewParams()
	rootParams.ChildSnippet = childSnippet
	code, err := expandAccessorMutatorTemlate(sliceReadRoot, params)
	sess.addAccessorMutatorSnippet(code)
	fmt.Printf("%v, %v\n", code, err)

	return err
}
