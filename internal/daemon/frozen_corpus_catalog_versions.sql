SELECT (SELECT data_version FROM pragma_data_version),
       (SELECT schema_version FROM pragma_schema_version)
