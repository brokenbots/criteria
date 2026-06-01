"""Schema generation helpers.

`pydantic_to_schema` reflects over a Pydantic v2 `BaseModel` and produces the
SDK `AdapterSchemaProto` shape.
"""

from typing import Any, Dict, Optional, Type, get_origin, get_args

from criteria.v2 import adapter_pb2


def pydantic_to_schema(model: Type[Any]) -> adapter_pb2.AdapterSchemaProto:
    """Convert a Pydantic v2 BaseModel into an AdapterSchemaProto.

    Supports scalar types (str, int, float, bool), lists of strings,
    and nested models (recursively). Optional fields are marked
    `required=False`; all others are `required=True`.
    """
    schema = adapter_pb2.AdapterSchemaProto()
    if not hasattr(model, "model_fields"):
        raise TypeError(f"expected a Pydantic v2 BaseModel, got {model!r}")

    for name, field_info in model.model_fields.items():
        field_proto = _field_info_to_proto(field_info)
        schema.fields[name].CopyFrom(field_proto)
    return schema


def _field_info_to_proto(field_info: Any) -> adapter_pb2.ConfigFieldProto:
    """Reflect a single Pydantic field_info into ConfigFieldProto."""
    proto = adapter_pb2.ConfigFieldProto()
    annotation = field_info.annotation

    # Handle Optional[T] = Union[T, None]
    is_optional = False
    origin = get_origin(annotation)
    args = get_args(annotation)

    if origin is not None:
        # Union or Optional
        if type(None) in args:
            is_optional = True
            # Pick the first non-None arg as the real type
            for arg in args:
                if arg is not type(None):
                    annotation = arg
                    break

    # Determine type string
    proto.type = _python_type_to_schema_type(annotation)
    proto.required = not is_optional and field_info.is_required()

    # Description from JSON schema extras or docstring
    if hasattr(field_info, "description") and field_info.description:
        proto.description = field_info.description

    # Default value
    if hasattr(field_info, "default") and field_info.default is not None:
        proto.default_str = str(field_info.default)

    return proto


def _python_type_to_schema_type(annotation: Any) -> str:
    """Map Python type annotations to schema type strings.

    Raises TypeError for unsupported or unhandled annotations.
    """
    if annotation is str:
        return "string"
    if annotation is int:
        return "number"
    if annotation is float:
        return "number"
    if annotation is bool:
        return "bool"

    origin = get_origin(annotation)
    args = get_args(annotation)

    # list[str]
    if origin is list:
        if args and args[0] is str:
            return "list_string"
        raise TypeError(
            f"unsupported list element type: {args[0]!r} (only list[str] is supported)"
        )

    # Nested model
    if hasattr(annotation, "model_fields"):
        return "object"

    raise TypeError(f"unsupported schema type annotation: {annotation!r}")


def dict_to_schema_proto(fields: Optional[Dict[str, Any]]) -> Optional[adapter_pb2.AdapterSchemaProto]:
    """Build an AdapterSchemaProto from a plain dict of field definitions.

    Each value may be a dict with keys: type, required, description, default, sensitive.
    """
    if fields is None:
        return None
    schema = adapter_pb2.AdapterSchemaProto()
    for key, defn in (fields or {}).items():
        if isinstance(defn, dict):
            proto = adapter_pb2.ConfigFieldProto(
                type=defn.get("type", "string"),
                required=defn.get("required", False),
                description=defn.get("description", ""),
                default_str=str(defn.get("default", "")),
                sensitive=defn.get("sensitive", False),
            )
            schema.fields[key].CopyFrom(proto)
    return schema
