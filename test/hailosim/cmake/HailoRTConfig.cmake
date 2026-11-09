get_filename_component(_hailosim_prefix "${CMAKE_CURRENT_LIST_DIR}/../../.." ABSOLUTE)

if(NOT TARGET HailoRT::libhailort)
    add_library(HailoRT::libhailort SHARED IMPORTED)
    set_target_properties(HailoRT::libhailort PROPERTIES
        IMPORTED_LOCATION "${_hailosim_prefix}/lib/libhailort.so"
        INTERFACE_INCLUDE_DIRECTORIES "${_hailosim_prefix}/include")
endif()

set(HailoRT_FOUND TRUE)
