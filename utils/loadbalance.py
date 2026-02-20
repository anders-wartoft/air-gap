def gen_filters(pipes :int, redundancies :int):
    assert pipes > 0
    assert redundancies >= 0
    assert redundancies < pipes
    redundancys = (pipes-redundancies) % (pipes+1)
    filters = [[] for _ in range(pipes)]
    groups = []
    for _ in range(3):
        group_start = 0 if groups == [] else int(groups[-1][0])
        groups = []

        for i in range(group_start+1, group_start+pipes+1):
            groups.append([i])

        for group_depth in range(min(pipes - redundancys, pipes)):
            for group_index, _ in enumerate(groups):
                i = (group_index+group_depth+1) % (len(groups))
                add_group = groups[i]
                groups[group_index].append(add_group[0])
        for i, _ in enumerate(filters):
            filters[i] += groups[i]
    return filters

def compare_print(filters: list[list[int]]):
    # Determine full value range
    all_values = sorted(set().union(*filters))

    max_width = max(len(str(v)) for v in all_values)

    for f in filters:
        row = []
        f_set = set(f)  # O(1) lookup
        for v in all_values:
            if v in f_set:
                row.append(f"{v:>{max_width}}")
            else:
                row.append(" " * max_width)
        print(", ".join(row))

def prod_print(filters :list):
    for f in filters:
        f = sorted(f)
        print(",".join(str(n) for n in f))

pipes = int(input("How many pipelines do you have? "))
red = int(input("How many should be redundant? "))

filters = gen_filters(pipes,red)
print("Pretty: ")
compare_print(filters)

print()
print("Heres the filters: ")
prod_print(filters)
