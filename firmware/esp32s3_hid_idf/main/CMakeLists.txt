idf_component_register(SRCS "main.c"
                       INCLUDE_DIRS ".")

# Load Wi-Fi credentials from .env (project root), injected as compile defines.
# Keep secrets out of sdkconfig/git; if .env is absent, the Kconfig defaults win.
set(ENV_FILE "${CMAKE_SOURCE_DIR}/.env")
if(EXISTS "${ENV_FILE}")
    # Re-run CMake when .env changes.
    set_property(DIRECTORY APPEND PROPERTY CMAKE_CONFIGURE_DEPENDS "${ENV_FILE}")
    file(STRINGS "${ENV_FILE}" _env_lines)
    foreach(_line ${_env_lines})
        if(_line MATCHES "^[ \t]*#" OR NOT _line MATCHES "=")
            continue()
        endif()
        if(_line MATCHES "^[ \t]*([A-Za-z_][A-Za-z0-9_]*)[ \t]*=[ \t]*(.*)$")
            set(_key "${CMAKE_MATCH_1}")
            set(_val "${CMAKE_MATCH_2}")
            string(REGEX REPLACE "^\"(.*)\"$" "\\1" _val "${_val}")  # strip "quotes"
            string(REGEX REPLACE "^'(.*)'$" "\\1" _val "${_val}")    # strip 'quotes'
            if(_key STREQUAL "WIFI_SSID" OR _key STREQUAL "WIFI_PASSWORD")
                target_compile_definitions(${COMPONENT_LIB} PRIVATE ${_key}="${_val}")
                message(STATUS "hidbridge: ${_key} loaded from .env")
            endif()
        endif()
    endforeach()
endif()
