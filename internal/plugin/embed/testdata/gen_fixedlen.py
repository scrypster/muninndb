# Generates the two tiny synthetic ONNX encoders used by local_padding_test.go.
# Neither is a real model: token id mod 16 indexes a 16x4 table, so the output
# is [batch, seq, 4]. Both only work at sequence length 8.
#   fixedlen8_declared.onnx  declares the fixed length on its inputs
#   fixedlen8_hidden.onnx    declares a dynamic axis but reshapes to length 8
# Regenerate: python3 gen_fixedlen.py (needs numpy and onnx).
import numpy as np, onnx
from onnx import helper as h, TensorProto as T, numpy_helper as nh
L, D = 8, 4
table = nh.from_array(np.arange(1, 16 * D + 1, dtype=np.float32).reshape(16, D) / 64.0, "table")
sixteen = nh.from_array(np.array(16, dtype=np.int64), "sixteen")
shape = nh.from_array(np.array([-1, L, D], dtype=np.int64), "shape")
def build(seq, path):
    nodes = [h.make_node("Mod", ["input_ids", "sixteen"], ["ids16"]),
             h.make_node("Gather", ["table", "ids16"], ["g"]),
             h.make_node("Reshape", ["g", "shape"], ["last_hidden_state"])]
    ins = [h.make_tensor_value_info(n, T.INT64, ["batch", seq]) for n in ("input_ids", "attention_mask")]
    out = [h.make_tensor_value_info("last_hidden_state", T.FLOAT, ["batch", seq, D])]
    m = h.make_model(h.make_graph(nodes, "fixedlen", ins, out, [table, sixteen, shape]),
                     opset_imports=[h.make_opsetid("", 17)])
    m.ir_version = 8
    onnx.checker.check_model(m)
    onnx.save(m, path)
build(L, "fixedlen8_declared.onnx")
build("seq", "fixedlen8_hidden.onnx")
