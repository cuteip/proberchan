.PHONY: gen
gen:
	rm -rf ./gen
	buf generate
